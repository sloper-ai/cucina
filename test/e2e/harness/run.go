// SPDX-License-Identifier: FSL-1.1-ALv2

package harness

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"runtime/debug"
	"time"
)

// Runner executes scenarios against one environment.
type Runner struct {
	Registry *Registry
	Env      *Env
	Governor *Governor
	// NewServices builds the concrete clients lazily (only for scenarios that
	// pass preflight). Nil means scenarios get no Services.
	NewServices func(ctx context.Context, env *Env) (Services, error)
	// ResultsDir receives result-<id>.json files (and the budget ledger).
	ResultsDir string
	Log        *slog.Logger
	Now        func() time.Time

	services Services
}

// Preflight returns "" if the scenario may run, else the SKIP reason. prior
// holds earlier results of this run (for DependsOn).
func (r *Runner) Preflight(s *Scenario, prior map[string]*Result) string {
	env := r.Env
	if !s.AllowedIn(env.Kind) {
		return fmt.Sprintf("scenario does not run in %s environments (allowed: %v)", env.Kind, allowedKinds(s))
	}
	if env.Kind == EnvProdSmoke && !s.ReadOnly {
		return "prod-smoke runs read-only scenarios only"
	}
	for _, req := range s.Requires {
		if !env.Has(req) {
			return fmt.Sprintf("requires %s, which environment %q does not provide", req, env.Name)
		}
	}
	if s.MaxInstances > 0 && env.Safety.MaxInstances > 0 && s.MaxInstances > env.Safety.MaxInstances {
		return fmt.Sprintf("may run %d instances, above the safety limit of %d", s.MaxInstances, env.Safety.MaxInstances)
	}
	for _, dep := range s.DependsOn {
		p, ok := prior[dep]
		switch {
		case !ok:
			return fmt.Sprintf("depends on %s, which has not run in run %s", dep, env.RunID)
		case p.Status != StatusPass:
			return fmt.Sprintf("depends on %s, which ended %s", dep, p.Status)
		}
	}
	if r.Governor != nil && env.Has(RequiresAWS) {
		if d := r.Governor.Decide(s, env.Safety); !d.Admit {
			return d.Reason
		}
	}
	return ""
}

func allowedKinds(s *Scenario) []EnvKind {
	if len(s.Envs) == 0 {
		return []EnvKind{EnvAWS}
	}
	return s.Envs
}

// Run executes one scenario by ID and saves its result.
func (r *Runner) Run(ctx context.Context, id string) (*Result, error) {
	s, ok := r.Registry.Get(id)
	if !ok {
		return nil, fmt.Errorf("unknown scenario %q (known: %v)", id, r.Registry.Sorted())
	}
	prior, err := r.prior()
	if err != nil {
		return nil, err
	}
	now := r.now
	res := &Result{
		ID: s.ID, Title: s.Title, Env: r.Env.Name, EnvKind: r.Env.Kind, RunID: r.Env.RunID,
		Started: now(), CostClass: s.Cost, Cost: CostRecord{EstimateUSD: s.EstimateUSD},
	}
	if r.Env.AWS != nil {
		res.Tags = maps.Clone(r.Env.AWS.Tags)
	}
	finish := func(st Status) (*Result, error) {
		res.Status = st
		res.Finished = now()
		res.Duration = res.Finished.Sub(res.Started)
		if r.ResultsDir != "" {
			if _, err := res.Save(r.ResultsDir); err != nil {
				return res, err
			}
		}
		return res, nil
	}
	if reason := r.Preflight(s, prior); reason != "" {
		res.SkipReason = reason
		r.log().Info("scenario skipped", "scenario", s.ID, "reason", reason)
		return finish(StatusSkip)
	}
	if r.services == nil && r.NewServices != nil {
		svc, err := r.NewServices(ctx, r.Env)
		if err != nil {
			res.Error = "services: " + err.Error()
			return finish(StatusError)
		}
		r.services = svc
	}

	runCtx, cancel := context.WithTimeout(ctx, s.Timeout)
	defer cancel()
	c := &Context{
		Context: runCtx, Env: r.Env, Scenario: s, Result: res, Services: r.services,
		Log: r.log().With("scenario", s.ID), Now: now, Prior: prior,
	}
	runErr := safeRun(s.Run, c)
	status := Classify(runErr)
	if runErr != nil {
		var se *SkipError
		if errors.As(runErr, &se) {
			res.SkipReason = se.Reason
		} else {
			res.Error = runErr.Error()
		}
	}
	if status != StatusSkip {
		// Post-conditions run even after a failure (diagnostics), each with
		// its own bound so one hung query cannot eat the report.
		for _, chk := range s.Post {
			pctx, pcancel := context.WithTimeout(ctx, 2*time.Minute)
			cr := chk.Evaluate(pctx, c)
			pcancel()
			res.Checks = append(res.Checks, cr)
		}
		if status == StatusPass {
			for _, cr := range res.Checks {
				if !cr.Pass && cr.Skipped == "" {
					status = StatusFail
					res.Error = fmt.Sprintf("post-condition %q failed: %s", cr.Name, cr.Detail)
					break
				}
			}
			for _, n := range res.NFRs {
				if !n.Pass && status == StatusPass {
					status = StatusFail
					res.Error = fmt.Sprintf("%s (%s) missed: measured %g %s, target %s", n.ID, n.Subject, n.Measured, n.Unit, n.Target)
				}
			}
		}
	}
	if r.Governor != nil && r.Env.Has(RequiresAWS) {
		if err := r.Governor.Record(s.ID, now(), s.EstimateUSD, res.Cost.MeasuredUSD); err != nil {
			res.Notes = append(res.Notes, "budget ledger: "+err.Error())
		}
	}
	return finish(status)
}

// RunAll runs scenarios in the given order (registration order if empty).
func (r *Runner) RunAll(ctx context.Context, ids []string) ([]*Result, error) {
	if len(ids) == 0 {
		ids = r.Registry.IDs()
	}
	var out []*Result
	for _, id := range ids {
		res, err := r.Run(ctx, id)
		if err != nil {
			return out, err
		}
		out = append(out, res)
	}
	return out, nil
}

// Close releases the services.
func (r *Runner) Close() error {
	if r.services != nil {
		return r.services.Close()
	}
	return nil
}

func (r *Runner) prior() (map[string]*Result, error) {
	out := map[string]*Result{}
	if r.ResultsDir == "" {
		return out, nil
	}
	rs, err := LoadResults(r.ResultsDir)
	if err != nil {
		return nil, err
	}
	for _, x := range rs {
		if x.RunID == r.Env.RunID {
			out[x.ID] = x
		}
	}
	return out, nil
}

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Runner) log() *slog.Logger {
	if r.Log != nil {
		return r.Log
	}
	return slog.Default()
}

func safeRun(fn func(*Context) error, c *Context) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("panic: %v\n%s", p, debug.Stack())
		}
	}()
	return fn(c)
}
