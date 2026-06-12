// Package whenexpr owns the closed condition grammar for workflow phase- and
// job-level `when` fields.
//
// A `when` expression decides — server-side, at phase dispatch, before any
// Kubernetes Job exists — whether a declared phase or job runs in this run.
// False means zero compute is spent: the platform synthesizes durable skipped
// records instead of creating the Job (GitHub Actions `if:` / GitLab `rules:`
// parity). The workflow shape stays total and legible: skipped legs are
// declared, rendered, and attributed to the expression that skipped them.
//
// The grammar is deliberately closed — this is a routing condition, not a
// scripting surface:
//
//	when := "true" | "false" | term op term
//	op   := "==" | "!="
//	term := "${{ <ref> }}" | "'<literal>'" | bare-literal
//	ref  := "vars.<key>" | "inputs.<key>" | "run.preserve_test_env"
//
// Refs resolve against the registration's `vars` map, the run's dispatch
// inputs, and the closed set of run facts. Comparison is exact string
// equality after resolution. There is no truthiness, no boolean algebra, and
// no nesting: a condition that needs more than one comparison is a workflow
// shape problem, not a grammar gap.
package whenexpr

import (
	"fmt"
	"regexp"
	"strings"
)

// RunFacts is the closed set of run-state refs an expression may name.
// Extending it is a contract change: each fact must be a durable run fact
// available at phase dispatch (never mutable mid-run), or skip decisions
// would not be reconstructable from the run record.
var RunFacts = map[string]bool{
	"preserve_test_env": true,
}

var templatePattern = regexp.MustCompile(`^\$\{\{\s*([A-Za-z_][A-Za-z0-9_.-]*)\s*\}\}$`)
var bareLiteralPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// Expr is a parsed `when` expression.
type Expr struct {
	// Literal is set for the bare `true` / `false` forms.
	Literal *bool
	// Left/Right/Negate describe the comparison form: resolve(Left) ==
	// resolve(Right), inverted when Negate (the `!=` operator).
	Left   Term
	Right  Term
	Negate bool
	// Source is the original expression text, kept for skip attribution.
	Source string
}

// Term is one side of a comparison: either a template ref or a literal.
type Term struct {
	// Ref is the inside of `${{ ... }}` (e.g. "vars.feature_type") when the
	// term is a template; empty for literals.
	Ref string
	// Value is the literal text when the term is not a template.
	Value string
}

// Context carries the resolution context for evaluation.
type Context struct {
	// Vars is the workflow registration's vars map.
	Vars map[string]string
	// Inputs is the run's dispatch inputs (run_inputs).
	Inputs map[string]string
	// PreserveTestEnv is the run's preserve_test_env snapshot.
	PreserveTestEnv bool
}

// Parse parses a `when` expression. An empty (or all-whitespace) expression
// is valid and means "always run" (nil Expr, no error).
func Parse(raw string) (*Expr, error) {
	source := strings.TrimSpace(raw)
	if source == "" {
		return nil, nil
	}
	switch source {
	case "true":
		v := true
		return &Expr{Literal: &v, Source: source}, nil
	case "false":
		v := false
		return &Expr{Literal: &v, Source: source}, nil
	}
	op := ""
	idx := -1
	if i := strings.Index(source, "!="); i >= 0 {
		op, idx = "!=", i
	}
	if i := strings.Index(source, "=="); i >= 0 && (idx < 0 || i < idx) {
		op, idx = "==", i
	}
	if idx < 0 {
		return nil, fmt.Errorf("when %q: expected `true`, `false`, or `<term> ==|!= <term>`", source)
	}
	leftRaw := strings.TrimSpace(source[:idx])
	rightRaw := strings.TrimSpace(source[idx+len(op):])
	if strings.Contains(rightRaw, "==") || strings.Contains(rightRaw, "!=") {
		return nil, fmt.Errorf("when %q: exactly one comparison operator is allowed", source)
	}
	left, err := parseTerm(source, leftRaw)
	if err != nil {
		return nil, err
	}
	right, err := parseTerm(source, rightRaw)
	if err != nil {
		return nil, err
	}
	return &Expr{Left: left, Right: right, Negate: op == "!=", Source: source}, nil
}

func parseTerm(source, raw string) (Term, error) {
	if raw == "" {
		return Term{}, fmt.Errorf("when %q: empty comparison term", source)
	}
	if match := templatePattern.FindStringSubmatch(raw); match != nil {
		ref := match[1]
		if err := validateRefShape(source, ref); err != nil {
			return Term{}, err
		}
		return Term{Ref: ref}, nil
	}
	if strings.HasPrefix(raw, "'") && strings.HasSuffix(raw, "'") && len(raw) >= 2 {
		return Term{Value: raw[1 : len(raw)-1]}, nil
	}
	if strings.Contains(raw, "${{") {
		return Term{}, fmt.Errorf("when %q: malformed template term %q (expected `${{ vars.<key> }}`, `${{ inputs.<key> }}`, or `${{ run.<fact> }}`)", source, raw)
	}
	if !bareLiteralPattern.MatchString(raw) {
		return Term{}, fmt.Errorf("when %q: literal term %q must be single-quoted or match [A-Za-z0-9_.-]+", source, raw)
	}
	return Term{Value: raw}, nil
}

func validateRefShape(source, ref string) error {
	switch {
	case strings.HasPrefix(ref, "vars."):
		if strings.TrimPrefix(ref, "vars.") == "" {
			return fmt.Errorf("when %q: vars ref is missing a key", source)
		}
	case strings.HasPrefix(ref, "inputs."):
		if strings.TrimPrefix(ref, "inputs.") == "" {
			return fmt.Errorf("when %q: inputs ref is missing a key", source)
		}
	case strings.HasPrefix(ref, "run."):
		fact := strings.TrimPrefix(ref, "run.")
		if !RunFacts[fact] {
			return fmt.Errorf("when %q: run fact %q is not in the closed set (supported: preserve_test_env)", source, fact)
		}
	default:
		return fmt.Errorf("when %q: ref %q must start with vars., inputs., or run.", source, ref)
	}
	return nil
}

// Validate parses the expression and checks every ref against the declared
// vars and dispatch inputs, so a typo'd key fails at registration time with
// the exact location instead of at dispatch.
func Validate(raw, location string, declaredVars map[string]string, declaredInputs map[string]bool) error {
	expr, err := Parse(raw)
	if err != nil {
		return fmt.Errorf("%s: %w", location, err)
	}
	if expr == nil || expr.Literal != nil {
		return nil
	}
	for _, term := range []Term{expr.Left, expr.Right} {
		if term.Ref == "" {
			continue
		}
		switch {
		case strings.HasPrefix(term.Ref, "vars."):
			key := strings.TrimPrefix(term.Ref, "vars.")
			if _, ok := declaredVars[key]; !ok {
				return fmt.Errorf("%s: when %q refs ${{ vars.%s }} but the workflow vars map does not declare %q", location, expr.Source, key, key)
			}
		case strings.HasPrefix(term.Ref, "inputs."):
			key := strings.TrimPrefix(term.Ref, "inputs.")
			if !declaredInputs[key] {
				return fmt.Errorf("%s: when %q refs ${{ inputs.%s }} but dispatch_inputs does not declare %q", location, expr.Source, key, key)
			}
		}
	}
	return nil
}

// Eval resolves and evaluates the expression against the context. A nil
// expression evaluates true (always run). The second return value is a
// human-readable resolution trace ("${{ vars.feature_type }}='effect' !=
// 'effect' -> false") recorded as the skip reason so a skipped leg names
// exactly why it skipped.
func (e *Expr) Eval(ctx Context) (bool, string) {
	if e == nil {
		return true, ""
	}
	if e.Literal != nil {
		return *e.Literal, e.Source
	}
	leftVal := e.resolve(e.Left, ctx)
	rightVal := e.resolve(e.Right, ctx)
	result := leftVal == rightVal
	if e.Negate {
		result = !result
	}
	trace := fmt.Sprintf("%s -> %s %s %s -> %t",
		e.Source,
		quoteTrace(e.Left, leftVal),
		map[bool]string{false: "==", true: "!="}[e.Negate],
		quoteTrace(e.Right, rightVal),
		result,
	)
	return result, trace
}

func (e *Expr) resolve(term Term, ctx Context) string {
	if term.Ref == "" {
		return term.Value
	}
	switch {
	case strings.HasPrefix(term.Ref, "vars."):
		return ctx.Vars[strings.TrimPrefix(term.Ref, "vars.")]
	case strings.HasPrefix(term.Ref, "inputs."):
		return ctx.Inputs[strings.TrimPrefix(term.Ref, "inputs.")]
	case term.Ref == "run.preserve_test_env":
		if ctx.PreserveTestEnv {
			return "true"
		}
		return "false"
	}
	return ""
}

func quoteTrace(term Term, resolved string) string {
	if term.Ref != "" {
		return fmt.Sprintf("${{ %s }}='%s'", term.Ref, resolved)
	}
	return "'" + resolved + "'"
}
