package github

import (
	"context"
	"testing"
)

// markPullRequestReady must refuse an empty node id before issuing any API
// call — the GraphQL mutation requires the PR's global node id, and a blank one
// would otherwise produce a confusing upstream error mid-merge.
func TestMarkPullRequestReadyRequiresNodeID(t *testing.T) {
	c := &Client{}
	if err := c.markPullRequestReady(context.Background(), "   "); err == nil {
		t.Fatal("markPullRequestReady should reject an empty node id")
	}
}
