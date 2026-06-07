package server

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNativeGitHubPushPolicyTokenByCallbackTokenMintsIssueBranchPolicy(t *testing.T) {
	store := &fakeCompletionStore{
		tokenRunID:   "run-abc",
		tokenProject: "ambience",
		run: &RunReplayData{
			ID:          "run-abc",
			Project:     "ambience",
			IssueNumber: 168,
			IssueRepo:   "romaine-life/ambience",
		},
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/run-callbacks/cb/native/github-push-policy-token", nil)
	req.SetPathValue("callback_token", "cb")
	rec := httptest.NewRecorder()

	nativeGitHubPushPolicyTokenByCallbackToken(store, "signing-key")(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var result NativeGitHubPushPolicyTokenResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if result.Repo != "romaine-life/ambience" {
		t.Fatalf("repo=%q", result.Repo)
	}
	if result.Branch != "glimmung/issue-168/run-abc" {
		t.Fatalf("branch=%q", result.Branch)
	}
	if result.Ref != "refs/heads/glimmung/issue-168/run-abc" {
		t.Fatalf("ref=%q", result.Ref)
	}
	payload := decodeGitHubPolicyPayload(t, result.Token)
	for key, want := range map[string]string{
		"repo":    "romaine-life/ambience",
		"branch":  "glimmung/issue-168/run-abc",
		"ref":     "refs/heads/glimmung/issue-168/run-abc",
		"run_id":  "run-abc",
		"project": "ambience",
	} {
		if got, _ := payload[key].(string); got != want {
			t.Fatalf("payload[%s]=%q, want %q; payload=%v", key, got, want, payload)
		}
	}
	if got, _ := payload["issue_number"].(float64); got != 168 {
		t.Fatalf("payload issue_number=%v", payload["issue_number"])
	}
}

func TestNativeGitHubPushPolicyTokenByCallbackTokenRequiresSigningKey(t *testing.T) {
	store := &fakeCompletionStore{
		tokenRunID:   "run-abc",
		tokenProject: "ambience",
		run: &RunReplayData{
			ID:          "run-abc",
			Project:     "ambience",
			IssueNumber: 168,
			IssueRepo:   "romaine-life/ambience",
		},
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/run-callbacks/cb/native/github-push-policy-token", nil)
	req.SetPathValue("callback_token", "cb")
	rec := httptest.NewRecorder()

	nativeGitHubPushPolicyTokenByCallbackToken(store, "")(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func decodeGitHubPolicyPayload(t *testing.T, token string) map[string]any {
	t.Helper()
	body, _, ok := strings.Cut(token, ".")
	if !ok {
		t.Fatalf("token missing signature: %q", token)
	}
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		t.Fatalf("decode token body: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	return payload
}
