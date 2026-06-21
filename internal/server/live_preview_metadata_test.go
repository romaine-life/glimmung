package server

import (
	"reflect"
	"strings"
	"testing"
)

func TestLivePreviewConfigParsesSnakeAndCamel(t *testing.T) {
	for _, key := range []string{"live_preview", "livePreview"} {
		t.Run(key, func(t *testing.T) {
			project := Project{Metadata: map[string]any{
				key: map[string]any{
					"enabled":          true,
					"backend_prefixes": []any{"/api", "healthz/", "/api"},
				},
			}}
			cfg, enabled := livePreviewConfig(project)
			if !enabled {
				t.Fatalf("expected enabled")
			}
			// normalized: leading slash added, trailing slash stripped, dups dropped.
			want := []string{"/api", "/healthz"}
			if !reflect.DeepEqual(cfg.BackendPrefixes, want) {
				t.Fatalf("prefixes = %v, want %v", cfg.BackendPrefixes, want)
			}
		})
	}
}

func TestLivePreviewConfigAbsentOrDisabled(t *testing.T) {
	if _, ok := livePreviewConfig(Project{}); ok {
		t.Fatalf("absent key should report not-enabled")
	}
	project := Project{Metadata: map[string]any{"live_preview": map[string]any{"enabled": false}}}
	if _, ok := livePreviewConfig(project); ok {
		t.Fatalf("enabled=false should report not-enabled")
	}
}

func TestLivePreviewConfigParsesBackendPort(t *testing.T) {
	// jsonb decodes numbers as float64; a register payload may also send a string.
	for _, key := range []string{"backend_port", "backendPort"} {
		for name, raw := range map[string]any{"float64": float64(3000), "int": 3000, "string": "3000"} {
			t.Run(key+"/"+name, func(t *testing.T) {
				project := Project{Metadata: map[string]any{
					"live_preview": map[string]any{"enabled": true, key: raw},
				}}
				cfg, _ := livePreviewConfig(project)
				if cfg.BackendPort != 3000 {
					t.Fatalf("backend port = %d, want 3000", cfg.BackendPort)
				}
			})
		}
	}
	// Absent backend_port parses to 0 (provision falls back to the default).
	cfg, _ := livePreviewConfig(Project{Metadata: map[string]any{"live_preview": map[string]any{"enabled": true}}})
	if cfg.BackendPort != 0 {
		t.Fatalf("absent backend port = %d, want 0", cfg.BackendPort)
	}
}

func TestLivePreviewConfigDropsBareSlashPrefix(t *testing.T) {
	project := Project{Metadata: map[string]any{
		"live_preview": map[string]any{"enabled": true, "backend_prefixes": []any{"/", "/api"}},
	}}
	cfg, _ := livePreviewConfig(project)
	if !reflect.DeepEqual(cfg.BackendPrefixes, []string{"/api"}) {
		t.Fatalf("bare / should be dropped; got %v", cfg.BackendPrefixes)
	}
}

func TestValidateLivePreviewMetadata(t *testing.T) {
	cases := []struct {
		name    string
		meta    map[string]any
		wantErr string
	}{
		{"absent", nil, ""},
		{"valid", map[string]any{"live_preview": map[string]any{"enabled": true, "backend_prefixes": []any{"/api"}}}, ""},
		{"enabled wrong type", map[string]any{"live_preview": map[string]any{"enabled": 1}}, "enabled must be a boolean"},
		{"prefixes wrong type", map[string]any{"live_preview": map[string]any{"backend_prefixes": "/api"}}, "must be a list of strings"},
		{"prefix non-string", map[string]any{"live_preview": map[string]any{"backend_prefixes": []any{1}}}, "must be a list of strings"},
		{"bare slash", map[string]any{"live_preview": map[string]any{"backend_prefixes": []any{"/"}}}, "bare \"/\""},
		{"valid port", map[string]any{"live_preview": map[string]any{"backend_port": float64(3000)}}, ""},
		{"valid port camel", map[string]any{"live_preview": map[string]any{"backendPort": float64(8080)}}, ""},
		{"port out of range", map[string]any{"live_preview": map[string]any{"backend_port": float64(70000)}}, "valid TCP port"},
		{"port zero", map[string]any{"live_preview": map[string]any{"backend_port": float64(0)}}, "valid TCP port"},
		{"port non-integral", map[string]any{"live_preview": map[string]any{"backend_port": float64(80.5)}}, "must be an integer port"},
		{"port wrong type", map[string]any{"live_preview": map[string]any{"backend_port": []any{1}}}, "must be an integer port"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateLivePreviewMetadata(c.meta)
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, c.wantErr)
			}
		})
	}
}
