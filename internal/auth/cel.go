// SPDX-License-Identifier: FSL-1.1-ALv2

package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"
	"cel.dev/cel-go/common/types/traits"
	"cel.dev/cel-go/ext"
)

// CELLimits bound every TrustPolicy expression (R-AUTH-2): compile-time size and
// nesting, run-time cost, interrupt checks and a per-evaluation timeout. Any limit hit
// is an evaluation error, and every evaluation error fails closed.
type CELLimits struct {
	// MaxExpressionLen is the parser size limit in code points (default 4096).
	MaxExpressionLen int
	// MaxNesting is the parser recursion limit (default 64).
	MaxNesting int
	// CostLimit is the run-time cost budget per evaluation (default 1,000,000, the
	// per-call limit Kubernetes uses for CEL).
	CostLimit uint64
	// InterruptCheckFrequency is how many comprehension iterations run between context
	// checks (default 100).
	InterruptCheckFrequency uint
	// Timeout bounds one evaluation (default 100 ms).
	Timeout time.Duration
}

func (l CELLimits) withDefaults() CELLimits {
	if l.MaxExpressionLen <= 0 {
		l.MaxExpressionLen = 4096
	}
	if l.MaxNesting <= 0 {
		l.MaxNesting = 64
	}
	if l.CostLimit == 0 {
		l.CostLimit = 1_000_000
	}
	if l.InterruptCheckFrequency == 0 {
		l.InterruptCheckFrequency = 100
	}
	if l.Timeout <= 0 {
		l.Timeout = 100 * time.Millisecond
	}
	return l
}

// resultKind is the type an expression must produce.
type resultKind int

const (
	wantBool resultKind = iota
	wantString
	wantStringList
)

func (k resultKind) String() string {
	return [...]string{"bool", "string", "list(string)"}[k]
}

// celEnvs holds the two environments: rules and mappings see only `claims`; grant
// conditions also see the mapped `subject` and `groups`.
type celEnvs struct {
	claims *cel.Env
	grant  *cel.Env
	limits CELLimits
}

func newCELEnvs(limits CELLimits) (*celEnvs, error) {
	limits = limits.withDefaults()
	common := []cel.EnvOption{
		cel.Variable("claims", cel.MapType(cel.StringType, cel.DynType)),
		ext.Strings(),
		cel.CrossTypeNumericComparisons(true),
		cel.ParserExpressionSizeLimit(limits.MaxExpressionLen),
		cel.ParserRecursionLimit(limits.MaxNesting),
	}
	claimsEnv, err := cel.NewEnv(common...)
	if err != nil {
		return nil, err
	}
	grantEnv, err := cel.NewEnv(append(common,
		cel.Variable("subject", cel.StringType),
		cel.Variable("groups", cel.ListType(cel.StringType)),
	)...)
	if err != nil {
		return nil, err
	}
	return &celEnvs{claims: claimsEnv, grant: grantEnv, limits: limits}, nil
}

// program is one compiled expression.
type program struct {
	src  string
	prg  cel.Program
	want resultKind
	lim  CELLimits
}

// compile checks syntax, types and the result type at load time.
func (e *celEnvs) compile(env *cel.Env, expr string, want resultKind) (*program, error) {
	if expr == "" {
		return nil, errors.New("expression is empty")
	}
	ast, iss := env.Compile(expr)
	if iss.Err() != nil {
		return nil, fmt.Errorf("compiling %q: %w", expr, iss.Err())
	}
	out := ast.OutputType()
	ok := out.IsExactType(cel.DynType)
	switch want {
	case wantBool:
		ok = ok || out.IsExactType(cel.BoolType)
	case wantString:
		ok = ok || out.IsExactType(cel.StringType)
	case wantStringList:
		ok = ok || out.IsExactType(cel.ListType(cel.StringType)) || out.IsExactType(cel.ListType(cel.DynType))
	}
	if !ok {
		return nil, fmt.Errorf("expression %q has type %s, want %s", expr, out, want)
	}
	prg, err := env.Program(ast,
		cel.CostLimit(e.limits.CostLimit),
		cel.InterruptCheckFrequency(e.limits.InterruptCheckFrequency),
	)
	if err != nil {
		return nil, fmt.Errorf("planning %q: %w", expr, err)
	}
	return &program{src: expr, prg: prg, want: want, lim: e.limits}, nil
}

// eval runs the program under the cost limit and timeout. Any error, and any result of
// the wrong type, is returned as an error (callers fail closed).
func (p *program) eval(ctx context.Context, vars map[string]any) (ref.Val, error) {
	ctx, cancel := context.WithTimeout(ctx, p.lim.Timeout)
	defer cancel()
	out, _, err := p.prg.ContextEval(ctx, vars)
	if err != nil {
		return nil, err
	}
	if types.IsError(out) || types.IsUnknown(out) {
		return nil, fmt.Errorf("evaluation failed: %v", out)
	}
	return out, nil
}

func (p *program) evalBool(ctx context.Context, vars map[string]any) (bool, error) {
	out, err := p.eval(ctx, vars)
	if err != nil {
		return false, err
	}
	b, ok := out.(types.Bool)
	if !ok {
		return false, fmt.Errorf("expression returned %s, want bool", out.Type())
	}
	return bool(b), nil
}

func (p *program) evalString(ctx context.Context, vars map[string]any) (string, error) {
	out, err := p.eval(ctx, vars)
	if err != nil {
		return "", err
	}
	s, ok := out.(types.String)
	if !ok {
		return "", fmt.Errorf("expression returned %s, want string", out.Type())
	}
	return string(s), nil
}

func (p *program) evalStringList(ctx context.Context, vars map[string]any) ([]string, error) {
	out, err := p.eval(ctx, vars)
	if err != nil {
		return nil, err
	}
	l, ok := out.(traits.Lister)
	if !ok {
		return nil, fmt.Errorf("expression returned %s, want list(string)", out.Type())
	}
	var res []string
	it := l.Iterator()
	for it.HasNext() == types.True {
		s, ok := it.Next().(types.String)
		if !ok {
			return nil, errors.New("list element is not a string")
		}
		res = append(res, string(s))
	}
	return res, nil
}

// claimsValue converts decoded JSON (json.Number numbers) into CEL-friendly Go values:
// integral numbers become int64, others float64.
func claimsValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, x := range t {
			out[k] = claimsValue(x)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = claimsValue(x)
		}
		return out
	case json.Number:
		if i, err := t.Int64(); err == nil {
			return i
		}
		if f, err := t.Float64(); err == nil && !math.IsInf(f, 0) {
			return f
		}
		return t.String()
	case float64:
		if t == math.Trunc(t) && math.Abs(t) < 1<<53 {
			return int64(t)
		}
		return t
	}
	return v
}
