package runnermcp

import (
	"sort"
	"testing"
)

func TestIsCatalogTool(t *testing.T) {
	if !IsCatalogTool(ToolUploadEvidence) {
		t.Fatalf("upload_evidence must be a known catalog tool")
	}
	if IsCatalogTool("definitely_not_a_tool") {
		t.Fatalf("unknown name must not be reported as a catalog tool")
	}
	if IsCatalogTool("") {
		t.Fatalf("empty name must not be reported as a catalog tool")
	}
}

func TestCatalogToolNamesSortedCopy(t *testing.T) {
	names := CatalogToolNames()
	if len(names) == 0 {
		t.Fatalf("catalog must expose at least one tool")
	}
	if !sort.StringsAreSorted(names) {
		t.Fatalf("CatalogToolNames must be sorted, got %v", names)
	}
	// Every catalog name must actually be a registerable tool name.
	for _, n := range names {
		if !IsCatalogTool(n) {
			t.Fatalf("CatalogToolNames returned %q which IsCatalogTool rejects", n)
		}
	}
	// The returned slice must be a copy: mutating it must not corrupt the catalog.
	names[0] = "mutated"
	if !IsCatalogTool(CatalogToolNames()[0]) {
		t.Fatalf("CatalogToolNames must return a defensive copy")
	}
}
