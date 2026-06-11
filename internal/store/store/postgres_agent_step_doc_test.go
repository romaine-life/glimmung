package store

import (
	"testing"

	server "github.com/romaine-life/glimmung/internal/server"
)

// The workflow registration round-trip must preserve agent.github_token —
// PR #757 added the field to the API spec and runner, but the storage doc
// dropped it on persist, so a registered opt-in silently vanished
// (observed re-registering ambience.default).
func TestAgentStepDocRoundTripsGithubToken(t *testing.T) {
	spec := &server.AgentStepSpec{Slot: "implementation", Prompt: "p", GithubToken: true}
	doc := agentStepDocFromSpec(spec)
	if doc == nil || !doc.GithubToken {
		t.Fatalf("doc=%#v", doc)
	}
	back := agentStepFromDoc(doc)
	if back == nil || !back.GithubToken {
		t.Fatalf("spec=%#v", back)
	}
	if back.Slot != "implementation" || back.Prompt != "p" {
		t.Fatalf("round trip lost fields: %#v", back)
	}
	if agentStepDocFromSpec(&server.AgentStepSpec{Slot: "test_plan"}).GithubToken {
		t.Fatal("github_token must default false")
	}
}
