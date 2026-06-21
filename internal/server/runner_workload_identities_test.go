package server

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

type fakeFederatedIdentityCredentialClient struct {
	current map[string][]FederatedIdentityCredential
	upserts []FederatedIdentityCredential
	deletes []FederatedIdentityCredentialRef
	err     error
}

func (c *fakeFederatedIdentityCredentialClient) UpsertFederatedIdentityCredential(_ context.Context, credential FederatedIdentityCredential) error {
	if c.err != nil {
		return c.err
	}
	c.upserts = append(c.upserts, credential)
	return nil
}

func (c *fakeFederatedIdentityCredentialClient) ListFederatedIdentityCredentials(_ context.Context, ref FederatedIdentityCredentialRef) ([]FederatedIdentityCredential, error) {
	if c.err != nil {
		return nil, c.err
	}
	return append([]FederatedIdentityCredential{}, c.current[ref.IdentityName]...), nil
}

func (c *fakeFederatedIdentityCredentialClient) DeleteFederatedIdentityCredential(_ context.Context, ref FederatedIdentityCredentialRef) error {
	if c.err != nil {
		return c.err
	}
	c.deletes = append(c.deletes, ref)
	return nil
}

func TestRunnerWorkloadIdentitiesReconcilesManagedCredentials(t *testing.T) {
	client := &fakeFederatedIdentityCredentialClient{
		current: map[string][]FederatedIdentityCredential{
			"tank-session-identity": {{
				FederatedIdentityCredentialRef: FederatedIdentityCredentialRef{
					SubscriptionID: "sub",
					ResourceGroup:  "infra",
					IdentityName:   "tank-session-identity",
					CredentialName: "tank-slot-4-session",
				},
				Issuer:    "https://issuer.example/",
				Subject:   "system:serviceaccount:tank-slot-4-sessions:tank-slot-4-session",
				Audiences: []string{defaultWorkloadIdentityAudience},
			}, {
				FederatedIdentityCredentialRef: FederatedIdentityCredentialRef{
					SubscriptionID: "sub",
					ResourceGroup:  "infra",
					IdentityName:   "tank-session-identity",
					CredentialName: "tank-operator-2-session",
				},
				Issuer:    "https://issuer.example/",
				Subject:   "system:serviceaccount:tank-operator-2-sessions:tank-operator-2-session",
				Audiences: []string{defaultWorkloadIdentityAudience},
			}},
		},
	}
	service := RunnerWorkloadIdentityService{
		Client: client,
		Issuer: "https://issuer.example/",
		Now:    func() time.Time { return time.Date(2026, 5, 13, 7, 0, 0, 0, time.UTC) },
	}
	project := Project{
		Name: "tank",
		Metadata: map[string]any{
			"runner_standby_dns": map[string]any{
				"slot_prefix": "tank-slot",
				"count":       float64(3),
			},
			"runner_standby_workload_identity": map[string]any{
				"enabled":        true,
				"subscription":   "sub",
				"resource_group": "infra",
				"count":          float64(3),
				"credentials": []any{
					map[string]any{
						"identity_name":   "tank-session-identity",
						"credential_name": "{slot_name}-session",
						"subject":         "system:serviceaccount:{slot_name}-sessions:{slot_name}-session",
					},
				},
			},
		},
	}

	status, err := service.ReconcileRunnerWorkloadIdentities(context.Background(), project)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if status.State != RunnerWorkloadIdentityStatusOK {
		t.Fatalf("status=%#v", status)
	}
	wantUpserts := []string{"tank-slot-1-session", "tank-slot-2-session", "tank-slot-3-session"}
	gotUpserts := make([]string, 0, len(client.upserts))
	for _, credential := range client.upserts {
		gotUpserts = append(gotUpserts, credential.CredentialName)
		if credential.Issuer != "https://issuer.example/" {
			t.Fatalf("issuer=%q", credential.Issuer)
		}
	}
	if !reflect.DeepEqual(gotUpserts, wantUpserts) {
		t.Fatalf("upserts=%#v want %#v", gotUpserts, wantUpserts)
	}
	gotDeletes := []string{}
	for _, deleted := range client.deletes {
		gotDeletes = append(gotDeletes, deleted.CredentialName)
	}
	if !reflect.DeepEqual(gotDeletes, []string{"tank-slot-4-session", "tank-operator-2-session"}) {
		t.Fatalf("deletes=%#v", client.deletes)
	}
	if len(status.ManagedCredentials) != 3 {
		t.Fatalf("managed=%#v", status.ManagedCredentials)
	}
	if len(status.Deleted) != 2 {
		t.Fatalf("deleted status=%#v", status.Deleted)
	}
}

func TestRunnerWorkloadIdentitiesDoesNotDeleteManualLookalikes(t *testing.T) {
	cfg := runnerWorkloadIdentityConfig{
		SubscriptionID: "sub",
		ResourceGroup:  "infra",
		Issuer:         "https://issuer.example/",
		SlotPrefix:     "tank-slot",
		Count:          1,
		Credentials: []runnerWorkloadIdentityCredentialTemplate{{
			IdentityName:   "tank-session-identity",
			CredentialName: "{slot_name}-session",
			Subject:        "system:serviceaccount:{slot_name}-sessions:{slot_name}-session",
			Audiences:      []string{defaultWorkloadIdentityAudience},
		}},
	}
	cases := []FederatedIdentityCredential{
		{
			FederatedIdentityCredentialRef: FederatedIdentityCredentialRef{IdentityName: "tank-session-identity", CredentialName: "tank-slot-2-other"},
			Subject:                        "system:serviceaccount:tank-slot-2-sessions:tank-slot-2-session",
		},
		{
			FederatedIdentityCredentialRef: FederatedIdentityCredentialRef{IdentityName: "tank-session-identity", CredentialName: "tank-slot-2-session"},
			Subject:                        "system:serviceaccount:tank-slot-2-sessions:manual",
		},
		{
			FederatedIdentityCredentialRef: FederatedIdentityCredentialRef{IdentityName: "other-identity", CredentialName: "tank-slot-2-session"},
			Subject:                        "system:serviceaccount:tank-slot-2-sessions:tank-slot-2-session",
		},
	}
	for _, credential := range cases {
		if _, ok := managedWorkloadIdentityCredentialIndex(credential, cfg); ok {
			t.Fatalf("manual credential matched managed template: %#v", credential)
		}
	}
}

func TestRunnerWorkloadIdentitiesSkipsUnchangedCredentials(t *testing.T) {
	client := &fakeFederatedIdentityCredentialClient{
		current: map[string][]FederatedIdentityCredential{
			"tank-session-identity": {
				{
					FederatedIdentityCredentialRef: FederatedIdentityCredentialRef{
						SubscriptionID: "sub",
						ResourceGroup:  "infra",
						IdentityName:   "tank-session-identity",
						CredentialName: "tank-slot-1-session",
					},
					Issuer:    "https://issuer.example/",
					Subject:   "system:serviceaccount:tank-slot-1-sessions:tank-slot-1-session",
					Audiences: []string{defaultWorkloadIdentityAudience},
				},
				{
					FederatedIdentityCredentialRef: FederatedIdentityCredentialRef{
						SubscriptionID: "sub",
						ResourceGroup:  "infra",
						IdentityName:   "tank-session-identity",
						CredentialName: "tank-slot-2-session",
					},
					Issuer:    "https://old-issuer.example/",
					Subject:   "system:serviceaccount:tank-slot-2-sessions:tank-slot-2-session",
					Audiences: []string{defaultWorkloadIdentityAudience},
				},
			},
		},
	}
	service := RunnerWorkloadIdentityService{
		Client: client,
		Issuer: "https://issuer.example/",
		Now:    func() time.Time { return time.Date(2026, 5, 13, 7, 0, 0, 0, time.UTC) },
	}
	project := Project{
		Name: "tank",
		Metadata: map[string]any{
			"runner_standby_dns": map[string]any{
				"slot_prefix": "tank-slot",
				"count":       float64(2),
			},
			"runner_standby_workload_identity": map[string]any{
				"enabled":        true,
				"subscription":   "sub",
				"resource_group": "infra",
				"count":          float64(2),
				"credentials": []any{
					map[string]any{
						"identity_name":   "tank-session-identity",
						"credential_name": "{slot_name}-session",
						"subject":         "system:serviceaccount:{slot_name}-sessions:{slot_name}-session",
					},
				},
			},
		},
	}

	status, err := service.ReconcileRunnerWorkloadIdentities(context.Background(), project)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if status.State != RunnerWorkloadIdentityStatusOK {
		t.Fatalf("status=%#v", status)
	}
	if len(client.upserts) != 1 || client.upserts[0].CredentialName != "tank-slot-2-session" {
		t.Fatalf("upserts=%#v", client.upserts)
	}
	if len(status.Upserted) != 1 || status.Upserted[0].CredentialName != "tank-slot-2-session" {
		t.Fatalf("upserted status=%#v", status.Upserted)
	}
	if len(client.deletes) != 0 {
		t.Fatalf("deletes=%#v", client.deletes)
	}
}

func TestRunnerWorkloadIdentitiesSkippedWhenDisabled(t *testing.T) {
	service := RunnerWorkloadIdentityService{Client: &fakeFederatedIdentityCredentialClient{}}
	status, err := service.ReconcileRunnerWorkloadIdentities(context.Background(), Project{Metadata: map[string]any{}})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if status.State != "" {
		t.Fatalf("status=%#v", status)
	}
}

func TestRunnerWorkloadIdentitiesReportsConfigErrors(t *testing.T) {
	service := RunnerWorkloadIdentityService{Client: &fakeFederatedIdentityCredentialClient{}}
	status, err := service.ReconcileRunnerWorkloadIdentities(context.Background(), Project{
		Name: "tank",
		Metadata: map[string]any{
			"runner_standby_dns": map[string]any{"slot_prefix": "tank-slot", "count": float64(1)},
			"runner_standby_workload_identity": map[string]any{
				"enabled": true,
			},
		},
	})
	if err == nil {
		t.Fatal("expected config error")
	}
	if status.State != RunnerWorkloadIdentityStatusFailed || status.LastError == nil {
		t.Fatalf("status=%#v", status)
	}
}

func TestRunnerWorkloadIdentitiesReportsClientErrors(t *testing.T) {
	service := RunnerWorkloadIdentityService{
		Client: &fakeFederatedIdentityCredentialClient{err: errors.New("boom")},
		Issuer: "https://issuer.example/",
	}
	status, err := service.ReconcileRunnerWorkloadIdentities(context.Background(), Project{
		Name: "tank",
		Metadata: map[string]any{
			"runner_standby_dns": map[string]any{"slot_prefix": "tank-slot", "count": float64(1)},
			"runner_standby_workload_identity": map[string]any{
				"enabled":        true,
				"subscription":   "sub",
				"resource_group": "infra",
				"credentials": []any{map[string]any{
					"identity_name":   "tank-session-identity",
					"credential_name": "{slot_name}-session",
					"subject":         "system:serviceaccount:{slot_name}-sessions:{slot_name}-session",
				}},
			},
		},
	})
	if err == nil {
		t.Fatal("expected client error")
	}
	if status.State != RunnerWorkloadIdentityStatusFailed || status.LastError == nil {
		t.Fatalf("status=%#v", status)
	}
}

// killMePreviewProject mirrors kill-me's metadata: workload identity with NO
// metadata issuer (so the service issuer fallback applies) and a
// {slot_name}/{namespace} credential template.
func killMePreviewProject() Project {
	return Project{
		ID:   "kill-me",
		Name: "kill-me",
		Metadata: map[string]any{
			"runner_standby_dns": map[string]any{
				"slot_prefix": "kill-me-slot",
				"count":       float64(1),
				"record_base": "kill-me.dev.romaine.life",
			},
			"runner_standby_workload_identity": map[string]any{
				"enabled":        true,
				"resource_group": "infra",
				"subscription":   "sub-123",
				"credentials": []any{
					map[string]any{
						"credential_name": "aks-{slot_name}",
						"identity_name":   "kill-me-identity",
						"subject":         "system:serviceaccount:{namespace}:infra-shared",
					},
				},
			},
		},
	}
}

// glimmungPreviewProject mirrors glimmung's metadata: workload identity WITH a
// metadata issuer and a {slot_name} credential template.
func glimmungPreviewProject() Project {
	return Project{
		ID:   "glimmung",
		Name: "glimmung",
		Metadata: map[string]any{
			"runner_standby_dns": map[string]any{
				"slot_prefix": "glimmung",
				"count":       float64(5),
				"record_base": "glimmung.dev.romaine.life",
			},
			"runner_standby_workload_identity": map[string]any{
				"enabled":        true,
				"resource_group": "glimmung",
				"subscription":   "sub-123",
				"issuer":         "https://westus2.oic.prod-aks.azure.com/abc/def/",
				"credentials": []any{
					map[string]any{
						"credential_name": "{slot_name}-infra-shared",
						"identity_name":   "glimmung-identity",
						"subject":         "system:serviceaccount:{slot_name}:infra-shared",
					},
				},
			},
		},
	}
}

func TestEnsurePreviewWorkloadIdentityUpsertsForPreviewNamespace(t *testing.T) {
	client := &fakeFederatedIdentityCredentialClient{}
	// kill-me has no metadata issuer; the service issuer is the fallback.
	service := RunnerWorkloadIdentityService{Client: client, Issuer: "https://issuer.example/"}

	if err := service.EnsurePreviewWorkloadIdentity(context.Background(), killMePreviewProject(), "smoke-killme"); err != nil {
		t.Fatalf("EnsurePreviewWorkloadIdentity: %v", err)
	}
	if len(client.upserts) != 1 {
		t.Fatalf("upserts = %d, want 1: %+v", len(client.upserts), client.upserts)
	}
	want := FederatedIdentityCredential{
		FederatedIdentityCredentialRef: FederatedIdentityCredentialRef{
			SubscriptionID: "sub-123",
			ResourceGroup:  "infra",
			IdentityName:   "kill-me-identity",
			CredentialName: "aks-smoke-killme",
		},
		Issuer:    "https://issuer.example/",
		Subject:   "system:serviceaccount:smoke-killme:infra-shared",
		Audiences: []string{defaultWorkloadIdentityAudience},
	}
	if !reflect.DeepEqual(client.upserts[0], want) {
		t.Fatalf("upsert mismatch:\n got %+v\nwant %+v", client.upserts[0], want)
	}
}

func TestEnsurePreviewWorkloadIdentityUsesMetadataIssuer(t *testing.T) {
	client := &fakeFederatedIdentityCredentialClient{}
	// The metadata issuer must win over the service fallback.
	service := RunnerWorkloadIdentityService{Client: client, Issuer: "https://wrong-fallback/"}

	if err := service.EnsurePreviewWorkloadIdentity(context.Background(), glimmungPreviewProject(), "smoke-glimmung"); err != nil {
		t.Fatalf("EnsurePreviewWorkloadIdentity: %v", err)
	}
	if len(client.upserts) != 1 {
		t.Fatalf("upserts = %d, want 1", len(client.upserts))
	}
	got := client.upserts[0]
	if got.CredentialName != "smoke-glimmung-infra-shared" ||
		got.Subject != "system:serviceaccount:smoke-glimmung:infra-shared" ||
		got.IdentityName != "glimmung-identity" ||
		got.Issuer != "https://westus2.oic.prod-aks.azure.com/abc/def/" {
		t.Fatalf("unexpected preview credential: %+v", got)
	}
}

func TestEnsurePreviewWorkloadIdentityNoopWithoutConfig(t *testing.T) {
	client := &fakeFederatedIdentityCredentialClient{}
	service := RunnerWorkloadIdentityService{Client: client, Issuer: "https://issuer.example/"}
	// chess-tactics / ambience have no runner_standby_workload_identity.
	project := Project{ID: "chess-tactics", Name: "chess-tactics", Metadata: map[string]any{}}

	if err := service.EnsurePreviewWorkloadIdentity(context.Background(), project, "smoke-chess"); err != nil {
		t.Fatalf("EnsurePreviewWorkloadIdentity: %v", err)
	}
	if len(client.upserts) != 0 {
		t.Fatalf("expected no upserts for a project without workload identity, got %d", len(client.upserts))
	}
}

func TestEnsurePreviewWorkloadIdentityRequiresClient(t *testing.T) {
	// A WI project with no Azure client must fail loudly rather than silently skip
	// federation (silent skip is the original crash-loop bug).
	service := RunnerWorkloadIdentityService{Issuer: "https://issuer.example/"}
	if err := service.EnsurePreviewWorkloadIdentity(context.Background(), killMePreviewProject(), "smoke-killme"); err == nil {
		t.Fatal("expected an error when the workload identity client is not configured")
	}
}

func TestRemovePreviewWorkloadIdentityDeletes(t *testing.T) {
	client := &fakeFederatedIdentityCredentialClient{}
	// Delete addresses the credential by ref and needs no issuer.
	service := RunnerWorkloadIdentityService{Client: client}

	if err := service.RemovePreviewWorkloadIdentity(context.Background(), killMePreviewProject(), "smoke-killme"); err != nil {
		t.Fatalf("RemovePreviewWorkloadIdentity: %v", err)
	}
	want := FederatedIdentityCredentialRef{
		SubscriptionID: "sub-123",
		ResourceGroup:  "infra",
		IdentityName:   "kill-me-identity",
		CredentialName: "aks-smoke-killme",
	}
	if len(client.deletes) != 1 || client.deletes[0] != want {
		t.Fatalf("deletes = %+v, want [%+v]", client.deletes, want)
	}
}

func TestRemovePreviewWorkloadIdentityNoopWithoutConfig(t *testing.T) {
	client := &fakeFederatedIdentityCredentialClient{}
	service := RunnerWorkloadIdentityService{Client: client}
	project := Project{ID: "chess-tactics", Name: "chess-tactics", Metadata: map[string]any{}}

	if err := service.RemovePreviewWorkloadIdentity(context.Background(), project, "smoke-chess"); err != nil {
		t.Fatalf("RemovePreviewWorkloadIdentity: %v", err)
	}
	if len(client.deletes) != 0 {
		t.Fatalf("expected no deletes for a project without workload identity, got %d", len(client.deletes))
	}
}

func TestReconcileRunnerWorkloadIdentitiesPreservesPreviewCredential(t *testing.T) {
	// The standby reconcile must NOT garbage-collect an ad-hoc preview credential.
	// Its slot name (smoke-killme) does not match the <prefix>-<n> standby pattern,
	// so it is not "managed" by the standby pool and must be left alone — otherwise
	// the standby loop would undo per-preview federation on its next pass.
	previewCred := FederatedIdentityCredential{
		FederatedIdentityCredentialRef: FederatedIdentityCredentialRef{
			SubscriptionID: "sub-123",
			ResourceGroup:  "infra",
			IdentityName:   "kill-me-identity",
			CredentialName: "aks-smoke-killme",
		},
		Issuer:    "https://issuer.example/",
		Subject:   "system:serviceaccount:smoke-killme:infra-shared",
		Audiences: []string{defaultWorkloadIdentityAudience},
	}
	client := &fakeFederatedIdentityCredentialClient{
		current: map[string][]FederatedIdentityCredential{"kill-me-identity": {previewCred}},
	}
	service := RunnerWorkloadIdentityService{Client: client, Issuer: "https://issuer.example/"}

	if _, err := service.ReconcileRunnerWorkloadIdentities(context.Background(), killMePreviewProject()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	for _, d := range client.deletes {
		if d.CredentialName == "aks-smoke-killme" {
			t.Fatalf("standby reconcile deleted the preview credential: %+v", d)
		}
	}
}

func TestEnsurePreviewWorkloadIdentityRefusesWhenCapExceeded(t *testing.T) {
	// Fill the identity to the Azure cap with unrelated credentials.
	existing := make([]FederatedIdentityCredential, maxFederatedIdentityCredentialsPerIdentity)
	for i := range existing {
		existing[i] = FederatedIdentityCredential{FederatedIdentityCredentialRef: FederatedIdentityCredentialRef{
			SubscriptionID: "sub-123", ResourceGroup: "infra", IdentityName: "kill-me-identity",
			CredentialName: fmt.Sprintf("existing-%d", i),
		}}
	}
	client := &fakeFederatedIdentityCredentialClient{
		current: map[string][]FederatedIdentityCredential{"kill-me-identity": existing},
	}
	service := RunnerWorkloadIdentityService{Client: client, Issuer: "https://issuer.example/"}

	err := service.EnsurePreviewWorkloadIdentity(context.Background(), killMePreviewProject(), "smoke-killme")
	if err == nil {
		t.Fatal("expected a cap-exceeded error")
	}
	if !strings.Contains(err.Error(), "cap") {
		t.Fatalf("error should explain the cap, got: %v", err)
	}
	if len(client.upserts) != 0 {
		t.Fatalf("must not upsert any credential when over cap, got %d", len(client.upserts))
	}
}

func TestEnsurePreviewWorkloadIdentityAllowsReprovisionAtCap(t *testing.T) {
	// At the cap, but the preview's OWN credential already exists: a re-provision
	// consumes no new slot and must be allowed (idempotent upsert).
	existing := make([]FederatedIdentityCredential, 0, maxFederatedIdentityCredentialsPerIdentity)
	for i := 0; i < maxFederatedIdentityCredentialsPerIdentity-1; i++ {
		existing = append(existing, FederatedIdentityCredential{FederatedIdentityCredentialRef: FederatedIdentityCredentialRef{
			IdentityName: "kill-me-identity", CredentialName: fmt.Sprintf("existing-%d", i),
		}})
	}
	existing = append(existing, FederatedIdentityCredential{
		FederatedIdentityCredentialRef: FederatedIdentityCredentialRef{
			SubscriptionID: "sub-123", ResourceGroup: "infra", IdentityName: "kill-me-identity",
			CredentialName: "aks-smoke-killme",
		},
		Subject: "system:serviceaccount:smoke-killme:infra-shared",
	})
	client := &fakeFederatedIdentityCredentialClient{
		current: map[string][]FederatedIdentityCredential{"kill-me-identity": existing},
	}
	service := RunnerWorkloadIdentityService{Client: client, Issuer: "https://issuer.example/"}

	if err := service.EnsurePreviewWorkloadIdentity(context.Background(), killMePreviewProject(), "smoke-killme"); err != nil {
		t.Fatalf("re-provision at cap (own credential present) must be allowed: %v", err)
	}
	if len(client.upserts) != 1 {
		t.Fatalf("expected 1 idempotent upsert, got %d", len(client.upserts))
	}
}

func TestReclaimOrphanedPreviewCredentials(t *testing.T) {
	mk := func(name, ns string) FederatedIdentityCredential {
		return FederatedIdentityCredential{
			FederatedIdentityCredentialRef: FederatedIdentityCredentialRef{
				SubscriptionID: "sub-123", ResourceGroup: "infra", IdentityName: "kill-me-identity",
				CredentialName: name,
			},
			Subject:   "system:serviceaccount:" + ns + ":infra-shared",
			Issuer:    "https://issuer.example/",
			Audiences: []string{defaultWorkloadIdentityAudience},
		}
	}
	standby := mk("aks-kill-me-slot-1", "kill-me-slot-1")     // standby-owned — must be left alone
	live := mk("aks-smoke-live", "smoke-live")                 // a live preview — desired
	orphan := mk("aks-smoke-orphan", "smoke-orphan")           // no live row — reclaim
	foreign := FederatedIdentityCredential{                    // not Glimmung-minted — must be left alone
		FederatedIdentityCredentialRef: FederatedIdentityCredentialRef{
			SubscriptionID: "sub-123", ResourceGroup: "infra", IdentityName: "kill-me-identity",
			CredentialName: "some-other-credential",
		},
		Subject: "system:serviceaccount:smoke-orphan:some-other-sa",
	}
	client := &fakeFederatedIdentityCredentialClient{
		current: map[string][]FederatedIdentityCredential{"kill-me-identity": {standby, live, orphan, foreign}},
	}
	service := RunnerWorkloadIdentityService{Client: client, Issuer: "https://issuer.example/"}

	reclaimed, err := service.reclaimOrphanedPreviewCredentials(context.Background(), killMePreviewProject(), map[string]bool{"smoke-live": true})
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if reclaimed != 1 {
		t.Fatalf("reclaimed=%d, want 1 (only the orphan)", reclaimed)
	}
	if len(client.deletes) != 1 || client.deletes[0].CredentialName != "aks-smoke-orphan" {
		t.Fatalf("deletes=%+v, want only aks-smoke-orphan (standby, live, and foreign preserved)", client.deletes)
	}
}
