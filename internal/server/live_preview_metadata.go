package server

import (
	"fmt"
	"strconv"
	"strings"
)

// LivePreviewMetadataKey is the per-project metadata key that opts an app into
// the live-preview lane. It is a NEW key, deliberately distinct from:
//
//   - the retired artifact-streaming hot-swap contract key (the one the guard
//     scripts/check-deleted-test-slot-hot-swap.mjs forbids); and
//   - the faithful image-deploy lane's `test_slot_helm` / `test_slot_deploy`
//     keys, which the validation slot path owns and which this never touches.
//
// Shape: live_preview { enabled: bool, backend_prefixes: []string }. Glimmung
// reads it to know an app supports preview and which request path prefixes the
// edge reverse-proxies to the app backend (everything else is served
// override-first / fresh-passthrough by the edge).
const LivePreviewMetadataKey = "live_preview"

// livePreviewSettings is the parsed `live_preview` project metadata.
type livePreviewSettings struct {
	// Enabled is whether the app opts into the live-preview lane.
	Enabled bool
	// BackendPrefixes are normalized request path prefixes the edge
	// reverse-proxies to the app backend (e.g. /api, /healthz). They feed the
	// edge's LIVE_PREVIEW_EDGE_BACKEND_PREFIXES at provision time.
	BackendPrefixes []string
	// BackendPort is the in-pod port the app's OWN backend listens on, which the
	// edge reverse-proxies to (glimmung :8000, kill-me/chess-tactics :3000,
	// ambience :8080). It feeds the edge's LIVE_PREVIEW_EDGE_UPSTREAM at provision
	// time so the upstream points at THIS app's backend, not a hardcoded port.
	// Zero means unset; the provision falls back to defaultPreviewBackendPort.
	BackendPort int
}

// livePreviewConfig reads a project's `live_preview` metadata (snake_case or
// camelCase, mirroring testSlotHelmConfig's dual spelling). The second return
// reports whether the key is present AND enabled, so callers can gate on a
// single value the way testSlotHelmConfig does.
func livePreviewConfig(project Project) (livePreviewSettings, bool) {
	raw, ok := mapFromMap(project.Metadata, LivePreviewMetadataKey)
	if !ok {
		raw, ok = mapFromMap(project.Metadata, "livePreview")
	}
	if !ok {
		return livePreviewSettings{}, false
	}
	settings := livePreviewSettings{
		Enabled:         boolConfigValue(raw, "enabled"),
		BackendPrefixes: normalizeLivePreviewBackendPrefixes(stringSliceFromMap(raw, "backend_prefixes", "backendPrefixes")),
		BackendPort:     firstPositiveInt(nonNegativeIntMapValue(raw, "backend_port"), nonNegativeIntMapValue(raw, "backendPort")),
	}
	return settings, settings.Enabled
}

// firstPositiveInt returns the first value > 0, else 0. Lets backendPort be read
// under either snake_case or camelCase without a second map lookup chain.
func firstPositiveInt(values ...int) int {
	for _, v := range values {
		if v > 0 {
			return v
		}
	}
	return 0
}

// normalizeLivePreviewBackendPrefixes trims, prepends a leading slash, strips
// trailing slashes, de-dups, and drops empties. A bare "/" is dropped (it would
// proxy every frontend path to the backend and disable the override) — the edge
// rejects it for the same reason (internal/livepreview normalizePrefixes).
func normalizeLivePreviewBackendPrefixes(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, raw := range in {
		p := strings.TrimSpace(raw)
		if p == "" {
			continue
		}
		if !strings.HasPrefix(p, "/") {
			p = "/" + p
		}
		p = strings.TrimRight(p, "/")
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

// validateLivePreviewMetadata rejects a structurally invalid `live_preview`
// metadata block at the project write surface (register_project), the same way
// validateTestSlotHelmMetadata / validateTestSlotDeployMetadata guard their
// keys. It does not require the key — absence is valid (the app simply doesn't
// support preview).
func validateLivePreviewMetadata(metadata map[string]any) error {
	if metadata == nil {
		return nil
	}
	raw, ok := mapFromMap(metadata, LivePreviewMetadataKey)
	if !ok {
		raw, ok = mapFromMap(metadata, "livePreview")
	}
	if !ok {
		return nil
	}
	if v, present := raw["enabled"]; present {
		switch v.(type) {
		case bool, string:
		default:
			return fmt.Errorf("%s.enabled must be a boolean", LivePreviewMetadataKey)
		}
	}
	for _, key := range []string{"backend_prefixes", "backendPrefixes"} {
		val, present := raw[key]
		if !present {
			continue
		}
		switch items := val.(type) {
		case []string:
			for _, s := range items {
				if err := validateLivePreviewPrefix(key, s); err != nil {
					return err
				}
			}
		case []any:
			for _, item := range items {
				s, ok := item.(string)
				if !ok {
					return fmt.Errorf("%s.%s must be a list of strings", LivePreviewMetadataKey, key)
				}
				if err := validateLivePreviewPrefix(key, s); err != nil {
					return err
				}
			}
		default:
			return fmt.Errorf("%s.%s must be a list of strings", LivePreviewMetadataKey, key)
		}
	}
	for _, key := range []string{"backend_port", "backendPort"} {
		val, present := raw[key]
		if !present {
			continue
		}
		port, ok := portFromAny(val)
		if !ok {
			return fmt.Errorf("%s.%s must be an integer port", LivePreviewMetadataKey, key)
		}
		if port < 1 || port > 65535 {
			return fmt.Errorf("%s.%s must be a valid TCP port in [1, 65535]", LivePreviewMetadataKey, key)
		}
	}
	return nil
}

// portFromAny coerces a metadata scalar (JSON-decoded number/string) to a port
// int, reporting ok=false for a non-numeric type or a non-integral float so the
// validator can reject it distinctly from an out-of-range value. jsonb decodes
// numbers as float64; a register payload may also carry a string. (The package's
// intFromAny truncates and can't signal "not a number", which validation needs.)
func portFromAny(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		// Reject non-integral floats (e.g. 80.5) — a port is a whole number.
		if n != float64(int(n)) {
			return 0, false
		}
		return int(n), true
	case string:
		parsed, err := strconv.Atoi(strings.TrimSpace(n))
		if err != nil {
			return 0, false
		}
		return parsed, true
	default:
		return 0, false
	}
}

func validateLivePreviewPrefix(key, prefix string) error {
	trimmed := strings.TrimSpace(prefix)
	if trimmed == "" {
		// Empty entries are dropped at parse time, not an error.
		return nil
	}
	if strings.TrimRight(trimmed, "/") == "" {
		return fmt.Errorf(
			"%s.%s must not contain a bare \"/\" prefix: it would proxy every frontend path to the backend and disable the override",
			LivePreviewMetadataKey, key,
		)
	}
	return nil
}
