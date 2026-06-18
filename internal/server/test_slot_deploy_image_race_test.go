package server

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// deployRaceLauncher builds a KubernetesRunLauncher whose fake apiserver
// completes every helm installer Job and serves slot pods whose image is chosen
// per pod-list GET by podImageFor(call). reconcilePosts counts override
// reconciles (POST .../jobs); podGets counts verify pod-list reads.
func deployRaceLauncher(t *testing.T, slotName string, podImageFor func(call int) string) (*KubernetesRunLauncher, *int, *int) {
	t.Helper()
	tokenPath := tempTokenFile(t)
	reconcilePosts := 0
	podGets := 0
	launcher := &KubernetesRunLauncher{
		Settings: Settings{
			K8sAPIHost:           "https://kube.test",
			K8sSATokenPath:       tokenPath,
			RunnerNamespace:      "glimmung-runs",
			RunnerServiceAccount: "glimmung-runner",
			RunnerJobTTLSeconds:  3600,
		},
		HTTPClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			body := `{}`
			switch {
			case req.Method == http.MethodPost && req.URL.Path == "/apis/batch/v1/namespaces/glimmung-runs/jobs":
				reconcilePosts++
			case req.Method == http.MethodGet && strings.Contains(req.URL.Path, "/jobs/glim-slot-apply-"):
				body = `{"status":{"conditions":[{"type":"Complete","status":"True"}]}}`
			case req.Method == http.MethodGet && req.URL.Path == "/api/v1/namespaces/"+slotName+"/pods":
				podGets++
				body = `{"items":[{"spec":{"containers":[{"image":"` + podImageFor(podGets) + `"}]}}]}`
			}
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
		})},
	}
	return launcher, &reconcilePosts, &podGets
}

func deployRaceLease(slotName string, leaseNumber int) (Lease, Project) {
	num := leaseNumber
	lease := Lease{
		Project:     "tank-operator",
		LeaseNumber: &num,
		State:       "claimed",
		Metadata:    map[string]any{"runner_slot_name": slotName, "runner_slot_index": "1"},
	}
	project := Project{
		Name:       "tank-operator",
		GitHubRepo: "romaine-life/tank-operator",
		Metadata:   map[string]any{"test_slot_helm": map[string]any{"enabled": true}},
	}
	return lease, project
}

// TestDeployImageToSlotReassertsOverrideWhenBaselineClobbers reproduces the
// flaky-deploy ordering hazard: a baseline (chart-default) reconcile lands on
// the shared <slot>-hot release *after* the deploy's override, so the first
// running-image verify observes the slot reverted to baseline. The deploy must
// NOT report that stale slot as deployed — it re-asserts the override and
// re-checks, and the override deterministically wins.
func TestDeployImageToSlotReassertsOverrideWhenBaselineClobbers(t *testing.T) {
	const slotName = "tank-operator-slot-1"
	const override = "ghcr.io/romaine-life/tank-operator:override-abc123"
	const baseline = "ghcr.io/romaine-life/tank-operator:baseline-main"

	// First verify sees baseline (activation's apply clobbered the override);
	// after the re-assert reconcile the slot runs the override.
	launcher, reconcilePosts, podGets := deployRaceLauncher(t, slotName, func(call int) string {
		if call <= 1 {
			return baseline
		}
		return override
	})
	lease, project := deployRaceLease(slotName, 71)

	if err := launcher.DeployImageToSlot(context.Background(), lease, project, fakeRunnerGitHubTokenMinter{token: "ghs_test"}, "abc123def456", override, "image.tag"); err != nil {
		t.Fatalf("DeployImageToSlot: %v", err)
	}
	if *reconcilePosts != 2 {
		t.Fatalf("expected 2 override reconciles (initial + one re-assert), got %d", *reconcilePosts)
	}
	if *podGets != 2 {
		t.Fatalf("expected 2 running-image verifies (mismatch then match), got %d", *podGets)
	}
}

// TestDeployImageToSlotPassesOnFirstVerifyWhenNoClobber proves the re-assert
// loop adds no extra work on the happy path: when the first verify already sees
// the override, the deploy reconciles and verifies exactly once.
func TestDeployImageToSlotPassesOnFirstVerifyWhenNoClobber(t *testing.T) {
	const slotName = "tank-operator-slot-2"
	const override = "ghcr.io/romaine-life/tank-operator:override-xyz"

	launcher, reconcilePosts, podGets := deployRaceLauncher(t, slotName, func(int) string { return override })
	lease, project := deployRaceLease(slotName, 72)

	if err := launcher.DeployImageToSlot(context.Background(), lease, project, fakeRunnerGitHubTokenMinter{token: "ghs_test"}, "abc123def456", override, "image.tag"); err != nil {
		t.Fatalf("DeployImageToSlot: %v", err)
	}
	if *reconcilePosts != 1 {
		t.Fatalf("expected exactly 1 reconcile on the no-clobber path, got %d", *reconcilePosts)
	}
	if *podGets != 1 {
		t.Fatalf("expected exactly 1 verify on the no-clobber path, got %d", *podGets)
	}
}

// TestDeployImageToSlotFailsAfterBoundedReassert proves the loop is bounded: a
// slot that never converges on the override (e.g. a value-key mismatch) fails
// loudly after deployImageReassertAttempts rather than re-asserting forever.
func TestDeployImageToSlotFailsAfterBoundedReassert(t *testing.T) {
	const slotName = "tank-operator-slot-3"
	const override = "ghcr.io/romaine-life/tank-operator:override-never"
	const baseline = "ghcr.io/romaine-life/tank-operator:baseline-stuck"

	launcher, reconcilePosts, _ := deployRaceLauncher(t, slotName, func(int) string { return baseline })
	lease, project := deployRaceLease(slotName, 73)

	err := launcher.DeployImageToSlot(context.Background(), lease, project, fakeRunnerGitHubTokenMinter{token: "ghs_test"}, "abc123def456", override, "image.tag")
	if err == nil {
		t.Fatal("expected DeployImageToSlot to fail when the override never lands")
	}
	if !strings.Contains(err.Error(), "verify") {
		t.Fatalf("expected a verify failure, got %v", err)
	}
	if *reconcilePosts != deployImageReassertAttempts {
		t.Fatalf("expected exactly %d bounded reconciles, got %d", deployImageReassertAttempts, *reconcilePosts)
	}
}

// TestAwaitInflightActivationSerializesDeploy proves the serialize primitive:
// awaitInflightActivation blocks until the in-flight activation's done channel
// closes, so the deploy's override reconcile is ordered strictly after
// activation's baseline reconcile on the shared <slot>-hot release.
func TestAwaitInflightActivationSerializesDeploy(t *testing.T) {
	key := "tank-operator:number:991"
	token := &testSlotActivation{cancel: func() {}, done: make(chan struct{})}
	testSlotActivations.Store(key, token)
	defer testSlotActivations.Delete(key)

	returned := make(chan bool, 1)
	go func() { returned <- awaitInflightActivation(context.Background(), key) }()

	select {
	case <-returned:
		t.Fatal("awaitInflightActivation returned before activation finished")
	case <-time.After(50 * time.Millisecond):
	}

	close(token.done)
	select {
	case got := <-returned:
		if !got {
			t.Fatal("expected awaitInflightActivation to report it awaited an activation")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("awaitInflightActivation did not return after activation finished")
	}
}

// TestAwaitInflightActivationNoopWhenAbsent proves the deploy does not block
// when no activation is in flight (cross-replica deploys, or activation already
// finished) — it falls through to the override reconcile + re-assert safety net.
func TestAwaitInflightActivationNoopWhenAbsent(t *testing.T) {
	if awaitInflightActivation(context.Background(), "tank-operator:number:404404") {
		t.Fatal("expected awaitInflightActivation to be a no-op when no activation is in flight")
	}
	if awaitInflightActivation(context.Background(), "") {
		t.Fatal("expected awaitInflightActivation to be a no-op for an empty key")
	}
}
