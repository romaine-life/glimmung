package runnermcp

import "sort"

// catalogNames is the canonical set of runner tool names a workflow job may
// request in its allow-list. It is the registration-time source of truth: a
// workflow that declares a tool outside this set is rejected before any run is
// dispatched, instead of failing late when the sidecar's Scoped() check rejects
// the name at pod start.
//
// The runtime Registry may expose a subset of the catalog — a tool whose
// backing dependency is unavailable in a given pod is not registered — but it
// never exposes a name outside the catalog. Adding a tool means adding its name
// here, so registration and runtime agree by construction.
var catalogNames = []string{
	ToolUploadEvidence,
}

// CatalogToolNames returns the known runner tool names in sorted order. Callers
// use it for registration validation and for human-facing "known tools" error
// messages; the returned slice is a copy and safe to mutate.
func CatalogToolNames() []string {
	out := make([]string, len(catalogNames))
	copy(out, catalogNames)
	sort.Strings(out)
	return out
}

// IsCatalogTool reports whether name is a known runner tool that a job may
// declare in its allow-list.
func IsCatalogTool(name string) bool {
	for _, n := range catalogNames {
		if n == name {
			return true
		}
	}
	return false
}
