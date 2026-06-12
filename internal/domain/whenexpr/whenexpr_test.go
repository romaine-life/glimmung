package whenexpr

import (
	"strings"
	"testing"
)

func TestParseEmptyMeansAlwaysRun(t *testing.T) {
	for _, raw := range []string{"", "   ", "\n"} {
		expr, err := Parse(raw)
		if err != nil {
			t.Fatalf("Parse(%q) error: %v", raw, err)
		}
		if expr != nil {
			t.Fatalf("Parse(%q) expected nil expr", raw)
		}
		ok, _ := expr.Eval(Context{})
		if !ok {
			t.Fatalf("nil expr must evaluate true")
		}
	}
}

func TestParseBooleanLiterals(t *testing.T) {
	expr, err := Parse("false")
	if err != nil {
		t.Fatal(err)
	}
	if ok, trace := expr.Eval(Context{}); ok || trace != "false" {
		t.Fatalf("false literal: got %v %q", ok, trace)
	}
	expr, err = Parse("true")
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := expr.Eval(Context{}); !ok {
		t.Fatal("true literal must evaluate true")
	}
}

func TestParseComparisons(t *testing.T) {
	cases := []struct {
		raw  string
		ctx  Context
		want bool
	}{
		{"${{ vars.feature_type }} != 'effect'", Context{Vars: map[string]string{"feature_type": "effect"}}, false},
		{"${{ vars.feature_type }} != 'effect'", Context{Vars: map[string]string{"feature_type": "stats-display"}}, true},
		{"${{ vars.feature_type }} == 'effect'", Context{Vars: map[string]string{"feature_type": "effect"}}, true},
		{"${{ run.preserve_test_env }} == 'false'", Context{PreserveTestEnv: false}, true},
		{"${{ run.preserve_test_env }} == 'false'", Context{PreserveTestEnv: true}, false},
		{"${{ inputs.git_ref }} == main", Context{Inputs: map[string]string{"git_ref": "main"}}, true},
		{"${{ inputs.git_ref }} != main", Context{Inputs: map[string]string{"git_ref": "feature"}}, true},
		// Unset refs resolve to empty string; comparison stays exact.
		{"${{ vars.missing }} == ''", Context{}, true},
	}
	for _, tc := range cases {
		expr, err := Parse(tc.raw)
		if err != nil {
			t.Fatalf("Parse(%q): %v", tc.raw, err)
		}
		got, trace := expr.Eval(tc.ctx)
		if got != tc.want {
			t.Fatalf("Eval(%q) = %v (trace %q), want %v", tc.raw, got, trace, tc.want)
		}
		if !strings.Contains(trace, "->") {
			t.Fatalf("Eval(%q) trace %q must carry the resolution", tc.raw, trace)
		}
	}
}

func TestParseRejectsOpenGrammar(t *testing.T) {
	rejected := []string{
		"yes",                                   // bare literal alone is not a condition
		"${{ vars.a }}",                         // truthiness is not supported
		"${{ vars.a }} == ${{ vars.b }} == 'x'", // one operator only
		"${{ secrets.token }} == 'x'",           // unknown namespace
		"${{ run.slot }} == 'x'",                // run fact outside the closed set
		"${{ vars. }} == 'x'",                   // missing key
		"a b == 'x'",                            // unquoted multi-word literal
		"${{vars.a} == 'x'",                     // malformed template
	}
	for _, raw := range rejected {
		if _, err := Parse(raw); err == nil {
			t.Fatalf("Parse(%q) must be rejected", raw)
		}
	}
}

func TestValidateNamesUndeclaredRefs(t *testing.T) {
	vars := map[string]string{"feature_type": "effect"}
	inputs := map[string]bool{"git_ref": true}

	if err := Validate("${{ vars.feature_type }} != 'effect'", "phase llm-work job llm-test-plan", vars, inputs); err != nil {
		t.Fatalf("declared vars ref must validate: %v", err)
	}
	if err := Validate("${{ inputs.git_ref }} == main", "loc", vars, inputs); err != nil {
		t.Fatalf("declared inputs ref must validate: %v", err)
	}
	if err := Validate("${{ run.preserve_test_env }} == 'false'", "loc", vars, inputs); err != nil {
		t.Fatalf("closed run fact must validate: %v", err)
	}

	err := Validate("${{ vars.nope }} == 'x'", "phase p job j", vars, inputs)
	if err == nil || !strings.Contains(err.Error(), "vars map does not declare \"nope\"") || !strings.Contains(err.Error(), "phase p job j") {
		t.Fatalf("undeclared vars ref must fail with location and key, got: %v", err)
	}
	err = Validate("${{ inputs.nope }} == 'x'", "loc", vars, inputs)
	if err == nil || !strings.Contains(err.Error(), "dispatch_inputs does not declare \"nope\"") {
		t.Fatalf("undeclared inputs ref must fail, got: %v", err)
	}
}

func TestEvalTraceAttributesSkip(t *testing.T) {
	expr, err := Parse("${{ vars.feature_type }} != 'effect'")
	if err != nil {
		t.Fatal(err)
	}
	got, trace := expr.Eval(Context{Vars: map[string]string{"feature_type": "effect"}})
	if got {
		t.Fatal("expected false")
	}
	for _, fragment := range []string{"vars.feature_type", "'effect'", "false"} {
		if !strings.Contains(trace, fragment) {
			t.Fatalf("trace %q missing %q", trace, fragment)
		}
	}
}
