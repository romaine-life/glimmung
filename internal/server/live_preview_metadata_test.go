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
