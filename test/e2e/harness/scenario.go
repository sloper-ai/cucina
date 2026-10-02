// SPDX-License-Identifier: FSL-1.1-ALv2

// Package harness is Cucina's scenario harness (R-TEST-8d): a registry of
// scenarios that each declare an ID, prerequisites, a cost class, a timeout
// and post-conditions written as SLI/invariant checks; an environment
// descriptor that supplies endpoints, capabilities and safety limits; SKIP
// with a reason for every unmet prerequisite; a budget governor; and one JSON
// result per scenario. The same harness runs on kind (fakes), in the
// temporary AWS environment, as a read-only production smoke subset and from
// the canary CronJob (the canaries share internal/canary).
//
// The package is pure: concrete clients (SSM, EC2, Prometheus, kubectl,
// cucinactl) are injected by test/e2e/infra.
package harness

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"time"
)

// Requirement is a scenario prerequisite. The four R-TEST-8d requirements
// come first; the others are finer-grained capabilities an environment
// descriptor may offer.
type Requirement string

const (
	RequiresAWS         Requirement = "aws"
	RequiresMacHost     Requirement = "mac-host"
	RequiresIdP         Requirement = "idp"
	RequiresDestructive Requirement = "destructive"

	RequiresKubernetes    Requirement = "kubernetes"
	RequiresPrometheus    Requirement = "prometheus"
	RequiresLinuxClient   Requirement = "linux-client"
	RequiresWindowsClient Requirement = "windows-client"
	RequiresCucinactl     Requirement = "cucinactl"
	RequiresCrossMatrix   Requirement = "cross-matrix" // platforms/targets.json from the cross agent
	RequiresHostdPkg      Requirement = "hostd-pkg"    // signed pkg + simulate-mdm kit (T14)
)

// CostClass is the coarse spend class a scenario records (§12).
type CostClass string

const (
	CostNone   CostClass = "none"   // no AWS spend beyond the standing environment
	CostLow    CostClass = "low"    // < $1
	CostMedium CostClass = "medium" // $1–$10
	CostHigh   CostClass = "high"   // > $10
)

// EnvKind is the kind of environment a descriptor describes.
type EnvKind string

const (
	EnvKindCluster EnvKind = "kind"       // local kind cluster with fakes; ≤ 2 min per scenario
	EnvAWS         EnvKind = "aws-e2e"    // the temporary AWS campaign environment (§10)
	EnvProdSmoke   EnvKind = "prod-smoke" // safe read-only subset against a real deployment
	EnvCanary      EnvKind = "canary"     // in-cluster canary CronJob
)

// Scenario is one registered scenario.
type Scenario struct {
	ID    string // e.g. "T1", "T9c", "canary-cache"
	Title string
	// Requires lists prerequisites; any unmet one yields SKIP with a reason.
	Requires []Requirement
	// Envs restricts the environment kinds the scenario may run in (empty:
	// aws-e2e only). prod-smoke additionally requires ReadOnly.
	Envs     []EnvKind
	ReadOnly bool
	Cost     CostClass
	// EstimateUSD is the projected spend the budget governor reserves.
	EstimateUSD float64
	// MaxInstances is the most worker/test instances the scenario may cause to
	// exist at once (checked against the descriptor's safety limit).
	MaxInstances int
	// Essential marks MUST criteria (the governor never drops them silently).
	Essential bool
	Timeout   time.Duration
	// DependsOn lists scenarios whose state this one builds on (T2 needs the
	// cache T1 filled); they must have passed in the same run.
	DependsOn []string
	// NFRs lists the NFR rows the scenario measures (report cross-reference).
	NFRs []string
	// Run performs the scenario. Record metrics and NFR measurements on c.
	Run func(c *Context) error
	// Post are post-conditions evaluated after Run (even if Run failed, for
	// diagnostics), each an SLI/invariant query.
	Post []Check
}

var idPattern = regexp.MustCompile(`^(T\d{1,2}[a-z]?|[a-z][a-z0-9-]*)$`)

// Validate checks a scenario declaration.
func (s *Scenario) Validate() error {
	switch {
	case !idPattern.MatchString(s.ID):
		return fmt.Errorf("scenario %q: invalid id", s.ID)
	case s.Title == "":
		return fmt.Errorf("scenario %s: missing title", s.ID)
	case s.Run == nil:
		return fmt.Errorf("scenario %s: missing Run", s.ID)
	case s.Timeout <= 0:
		return fmt.Errorf("scenario %s: missing timeout", s.ID)
	case s.Cost == "":
		return fmt.Errorf("scenario %s: missing cost class", s.ID)
	case s.EstimateUSD < 0:
		return fmt.Errorf("scenario %s: negative estimate", s.ID)
	}
	for _, e := range s.Envs {
		if e == EnvProdSmoke && !s.ReadOnly {
			return fmt.Errorf("scenario %s: prod-smoke scenarios must be read-only", s.ID)
		}
		if e == EnvKindCluster && s.Timeout > 2*time.Minute {
			return fmt.Errorf("scenario %s: kind scenarios must finish within 2 min (R-TEST-5.7)", s.ID)
		}
	}
	return nil
}

// AllowedIn reports whether the scenario may run in environments of kind k.
func (s *Scenario) AllowedIn(k EnvKind) bool {
	if len(s.Envs) == 0 {
		return k == EnvAWS
	}
	for _, e := range s.Envs {
		if e == k {
			return true
		}
	}
	return false
}

// Registry holds scenarios by ID.
type Registry struct {
	byID  map[string]*Scenario
	order []string
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{byID: map[string]*Scenario{}} }

// Register adds scenarios; it panics on invalid or duplicate declarations,
// which are programming errors caught by the registry test.
func (r *Registry) Register(ss ...*Scenario) {
	for _, s := range ss {
		if err := s.Validate(); err != nil {
			panic(err)
		}
		if _, dup := r.byID[s.ID]; dup {
			panic(fmt.Sprintf("scenario %s registered twice", s.ID))
		}
		r.byID[s.ID] = s
		r.order = append(r.order, s.ID)
	}
}

// Get returns a scenario by ID.
func (r *Registry) Get(id string) (*Scenario, bool) {
	s, ok := r.byID[id]
	return s, ok
}

// IDs returns the IDs in registration order (the campaign order).
func (r *Registry) IDs() []string { return append([]string(nil), r.order...) }

// Sorted returns IDs sorted naturally (T2 < T10).
func (r *Registry) Sorted() []string {
	ids := r.IDs()
	sort.Slice(ids, func(i, j int) bool { return naturalLess(ids[i], ids[j]) })
	return ids
}

func naturalLess(a, b string) bool {
	na, ra := splitID(a)
	nb, rb := splitID(b)
	if na != nb {
		return na < nb
	}
	return ra < rb
}

func splitID(id string) (int, string) {
	if len(id) < 2 || id[0] != 'T' {
		return 1 << 20, id
	}
	n, i := 0, 1
	for ; i < len(id) && id[i] >= '0' && id[i] <= '9'; i++ {
		n = n*10 + int(id[i]-'0')
	}
	return n, id[i:]
}

// Check is a post-condition: an SLI/invariant query over the shared slo/ and
// invariants/ definitions, a tag-filtered AWS describe, or a cucinactl JSON
// assertion.
type Check interface {
	CheckName() string
	Evaluate(ctx context.Context, c *Context) CheckResult
}

// CheckFunc adapts a function to Check.
type CheckFunc struct {
	Name string
	Kind string // "promql", "invariant", "aws", "cli", "custom"
	Fn   func(ctx context.Context, c *Context) CheckResult
}

// CheckName implements Check.
func (f CheckFunc) CheckName() string { return f.Name }

// Evaluate implements Check.
func (f CheckFunc) Evaluate(ctx context.Context, c *Context) CheckResult {
	r := f.Fn(ctx, c)
	if r.Name == "" {
		r.Name = f.Name
	}
	if r.Kind == "" {
		r.Kind = f.Kind
	}
	return r
}
