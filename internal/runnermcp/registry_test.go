package runnermcp

import (
	"context"
	"encoding/json"
	"testing"
)

func noopTool(name string) Tool {
	return Tool{
		Name:        name,
		Description: name + " desc",
		InputSchema: json.RawMessage(`{"type":"object"}`),
		Handler:     func(context.Context, json.RawMessage) (any, error) { return nil, nil },
	}
}

func toolNames(ts []Tool) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.Name
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func newPopulated() *Registry {
	r := NewRegistry()
	r.Register(noopTool("upload_evidence"))
	r.Register(noopTool("capture_video"))
	r.Register(noopTool("await_pr_checks"))
	return r
}

func TestRegistry_NamesSorted(t *testing.T) {
	r := newPopulated()
	got := r.Names()
	want := []string{"await_pr_checks", "capture_video", "upload_evidence"}
	if !equalStrings(got, want) {
		t.Fatalf("Names() = %v, want %v", got, want)
	}
}

func TestRegistry_ScopedExactSubsetSorted(t *testing.T) {
	r := newPopulated()
	got, err := r.Scoped([]string{"capture_video", "upload_evidence"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"capture_video", "upload_evidence"}
	if !equalStrings(toolNames(got), want) {
		t.Fatalf("Scoped() = %v, want %v (sorted)", toolNames(got), want)
	}
}

func TestRegistry_ScopedUnknownIsError(t *testing.T) {
	r := newPopulated()
	if _, err := r.Scoped([]string{"upload_evidence", "dispatch_run"}); err == nil {
		t.Fatal("expected error for unknown tool, got nil")
	}
}

func TestRegistry_ScopedEmptyYieldsEmpty(t *testing.T) {
	r := newPopulated()
	got, err := r.Scoped(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("empty allow-list should yield no tools, got %v", toolNames(got))
	}
}

func TestRegistry_ScopedDeduplicates(t *testing.T) {
	r := newPopulated()
	got, err := r.Scoped([]string{"capture_video", "capture_video"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0].Name != "capture_video" {
		t.Fatalf("duplicate names should collapse to one, got %v", toolNames(got))
	}
}

func TestRegistry_ScopedEmptyNameIsError(t *testing.T) {
	r := newPopulated()
	if _, err := r.Scoped([]string{""}); err == nil {
		t.Fatal("expected error for empty tool name in allow-list")
	}
}

func TestRegistry_RegisterPanics(t *testing.T) {
	cases := map[string]func(){
		"empty name":  func() { NewRegistry().Register(Tool{Handler: noopTool("x").Handler}) },
		"nil handler": func() { NewRegistry().Register(Tool{Name: "x"}) },
		"duplicate":   func() { r := NewRegistry(); r.Register(noopTool("x")); r.Register(noopTool("x")) },
	}
	for name, fn := range cases {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatalf("expected panic for %s", name)
				}
			}()
			fn()
		})
	}
}

func TestRegistry_Has(t *testing.T) {
	r := newPopulated()
	if !r.Has("capture_video") {
		t.Fatal("Has(capture_video) = false, want true")
	}
	if r.Has("nope") {
		t.Fatal("Has(nope) = true, want false")
	}
}
