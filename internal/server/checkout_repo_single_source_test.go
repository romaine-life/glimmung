package server

import (
	"strings"
	"testing"
)

// The primary checkout repo is a single-source-of-truth value derived from the
// project's github_repo (carried on the run as IssueRepo). These tests pin the
// workflow-execution invariant: a stale baked checkout.repo is overridden at
// launch, extra (cross-repo) checkouts are left author-controlled, and a
// non-empty primary checkout.repo is rejected at registration so the retired
// baked-literal path cannot return.

func TestDerivePrimaryCheckoutRepoOverridesStaleBakedRepo(t *testing.T) {
	phase := PhaseSpec{
		Name: "prepare",
		Jobs: []NativeJobSpec{
			{ID: "env-prep", Checkout: &NativeCheckoutSpec{Repo: "nelsong6/ambience", Ref: "main", Path: "/workspace/ambience"}},
			{ID: "managed-gate"}, // no checkout (managed primitive) — must stay untouched
			{
				ID:             "cross",
				Checkout:       &NativeCheckoutSpec{Repo: "nelsong6/ambience", Ref: "main"},
				ExtraCheckouts: []NativeCheckoutSpec{{Repo: "other-org/tool", Ref: "v1"}},
			},
		},
	}

	got, err := derivePrimaryCheckoutRepo(phase, "romaine-life/ambience")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Jobs[0].Checkout.Repo != "romaine-life/ambience" {
		t.Errorf("primary repo not derived: got %q", got.Jobs[0].Checkout.Repo)
	}
	if got.Jobs[0].Checkout.Ref != "main" || got.Jobs[0].Checkout.Path != "/workspace/ambience" {
		t.Errorf("ref/path must stay author-controlled: %+v", *got.Jobs[0].Checkout)
	}
	if got.Jobs[1].Checkout != nil {
		t.Errorf("managed job without a checkout must stay nil, got %+v", got.Jobs[1].Checkout)
	}
	if got.Jobs[2].Checkout.Repo != "romaine-life/ambience" {
		t.Errorf("job2 primary repo not derived: got %q", got.Jobs[2].Checkout.Repo)
	}
	if len(got.Jobs[2].ExtraCheckouts) != 1 || got.Jobs[2].ExtraCheckouts[0].Repo != "other-org/tool" {
		t.Errorf("extra checkouts must stay author-controlled: %+v", got.Jobs[2].ExtraCheckouts)
	}
	// Aliasing guard: the input phase must not be mutated.
	if phase.Jobs[0].Checkout.Repo != "nelsong6/ambience" {
		t.Errorf("input phase was mutated in place: %q", phase.Jobs[0].Checkout.Repo)
	}
}

func TestDerivePrimaryCheckoutRepoErrorsWhenIssueRepoMissing(t *testing.T) {
	phase := PhaseSpec{
		Name: "prepare",
		Jobs: []NativeJobSpec{{ID: "env-prep", Checkout: &NativeCheckoutSpec{Repo: "x/y"}}},
	}
	if _, err := derivePrimaryCheckoutRepo(phase, "   "); err == nil {
		t.Fatal("expected an error when a checkout is present but the run has no issue repo")
	}
}

func TestDerivePrimaryCheckoutRepoAllowsMissingIssueRepoWhenNoCheckouts(t *testing.T) {
	phase := PhaseSpec{
		Name: "evidence-gate",
		Jobs: []NativeJobSpec{{ID: "evidence-verification-gate"}},
	}
	got, err := derivePrimaryCheckoutRepo(phase, "")
	if err != nil {
		t.Fatalf("unexpected error for a phase with no checkouts: %v", err)
	}
	if got.Jobs[0].Checkout != nil {
		t.Errorf("checkout should remain nil, got %+v", got.Jobs[0].Checkout)
	}
}

func TestValidateWorkflowRegisterRejectsBakedPrimaryCheckoutRepo(t *testing.T) {
	req := WorkflowRegister{
		Name: "default",
		Phases: []PhaseSpec{{
			Name: "prepare",
			Kind: workflowKindNativeK8sJob,
			Jobs: []NativeJobSpec{{
				ID:       "env-prep",
				Checkout: &NativeCheckoutSpec{Repo: "nelsong6/ambience", Ref: "main", Path: "/workspace/ambience"},
			}},
		}},
	}

	err := ValidateWorkflowRegister(req)
	if err == nil {
		t.Fatal("expected registration to reject a baked primary checkout.repo")
	}
	if !strings.Contains(err.Error(), "checkout.repo") {
		t.Errorf("rejection should name checkout.repo, got: %v", err)
	}
}
