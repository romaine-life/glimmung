package pg

import "testing"

// TestStripServerManagedStatusRemovesOnlyStatusKeys is the core guard: authored
// config must survive, while reconciler-owned status keys must be removed
// so they never pollute authored config or its content hash.
func TestStripServerManagedStatusRemovesOnlyStatusKeys(t *testing.T) {
	in := map[string]any{
		"authored_build_config":                   map[string]any{"enabled": true},
		"test_slot_helm":                          map[string]any{"chart": "x"},
		"runner_standby_dns":                      map[string]any{"count": 2},
		"managed_auth_origin_status":              map[string]any{"state": "ok"},
		"runner_standby_workload_identity_status": map[string]any{"state": "ok"},
	}
	out := stripServerManagedStatus(in)

	for _, k := range []string{"authored_build_config", "test_slot_helm", "runner_standby_dns"} {
		if _, ok := out[k]; !ok {
			t.Errorf("authored key %q was dropped by stripServerManagedStatus", k)
		}
	}
	for _, k := range serverManagedProjectStatusKeys {
		if _, ok := out[k]; ok {
			t.Errorf("server-managed status key %q leaked into authored config", k)
		}
	}
	// Input must not be mutated (it is the caller's live map).
	if _, ok := in["managed_auth_origin_status"]; !ok {
		t.Error("stripServerManagedStatus mutated its input")
	}
}

// TestRoundTripRegisterPreservesAuthoredConfig encodes the regression directly:
// a caller reads a project (status merged into metadata), then re-registers
// passing that merged metadata back. The write path must keep authored config
// intact and must NOT persist reconciler status into authored config.
func TestRoundTripRegisterPreservesAuthoredConfig(t *testing.T) {
	authored := map[string]any{
		"authored_build_config": map[string]any{"enabled": true},
	}
	status := map[string]any{
		"managed_auth_origin_status": map[string]any{"state": "ok"},
	}
	// What a read returns to the client.
	merged := mergeStatusIntoMetadata(authored, status)
	if _, ok := merged["managed_auth_origin_status"]; !ok {
		t.Fatal("read merge did not surface reconciler status")
	}
	if _, ok := merged["authored_build_config"]; !ok {
		t.Fatal("read merge dropped authored config")
	}

	// What the write path stores when the client round-trips that metadata.
	stored := stripServerManagedStatus(merged)
	if _, ok := stored["authored_build_config"]; !ok {
		t.Error("round-trip register dropped authored_build_config")
	}
	if _, ok := stored["managed_auth_origin_status"]; ok {
		t.Error("round-trip register persisted reconciler status into authored config")
	}
}

// TestMergeStatusIntoMetadataStatusWins verifies the live reconciled value wins
// over any stale copy carried in authored metadata.
func TestMergeStatusIntoMetadataStatusWins(t *testing.T) {
	authored := map[string]any{"managed_auth_origin_status": "stale", "keep": 1}
	status := map[string]any{"managed_auth_origin_status": "live"}
	out := mergeStatusIntoMetadata(authored, status)
	if out["managed_auth_origin_status"] != "live" {
		t.Errorf("status did not win: got %v", out["managed_auth_origin_status"])
	}
	if out["keep"] != 1 {
		t.Errorf("authored-only key dropped: got %v", out["keep"])
	}
}

// TestProjectConfigSchemaRefDeterministic confirms the content hash is stable
// regardless of map insertion order and ignores absent vs empty metadata, and
// that different authored config yields a different ref.
func TestProjectConfigSchemaRefDeterministic(t *testing.T) {
	a := projectConfigSchemaRef("p", "o/r", map[string]any{"x": 1, "y": 2})
	b := projectConfigSchemaRef("p", "o/r", map[string]any{"y": 2, "x": 1})
	if a != b {
		t.Errorf("hash not order-independent: %q vs %q", a, b)
	}
	if nilRef, emptyRef := projectConfigSchemaRef("p", "o/r", nil), projectConfigSchemaRef("p", "o/r", map[string]any{}); nilRef != emptyRef {
		t.Errorf("nil vs empty metadata produced different refs: %q vs %q", nilRef, emptyRef)
	}
	if changed := projectConfigSchemaRef("p", "o/r", map[string]any{"x": 1, "y": 3}); changed == a {
		t.Error("changed authored config produced identical ref")
	}
	if repoChanged := projectConfigSchemaRef("p", "o/r2", map[string]any{"x": 1, "y": 2}); repoChanged == a {
		t.Error("changed github_repo produced identical ref")
	}
}

// TestProjectConfigSchemaRefIgnoresStatusKeys confirms that an authored config
// is hashed identically whether or not a round-tripped status key is present,
// because the write path strips status before hashing.
func TestProjectConfigSchemaRefIgnoresStatusKeys(t *testing.T) {
	authored := map[string]any{"authored_build_config": map[string]any{"enabled": true}}
	withStatus := mergeStatusIntoMetadata(authored, map[string]any{"managed_auth_origin_status": "ok"})

	clean := projectConfigSchemaRef("p", "o/r", stripServerManagedStatus(authored))
	roundTripped := projectConfigSchemaRef("p", "o/r", stripServerManagedStatus(withStatus))
	if clean != roundTripped {
		t.Errorf("status presence changed the config hash: %q vs %q", clean, roundTripped)
	}
}
