package step

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// retiredShellProducer matches the hand-rolled lib.sh producer surface this SDK
// replaces: the ~600-line near-identical `native_emit_output` /
// `native_emit_json_output` / `native_emit_abort` shell functions each consumer
// app used to fork. The SDK (this package) is the sanctioned step-producer
// surface; a shell producer fork reappearing inside glimmung is a migration
// regression.
var retiredShellProducer = regexp.MustCompile(`(?m)^\s*(function\s+)?native_emit_(output|json_output|abort)\s*\(`)

// TestSDKIsTheOnlySanctionedStepProducerSurface is the seed migration guard the
// later app slices will extend. It fails if any shell file inside the glimmung
// repository defines a retired native_emit_* producer function — proving the
// shell producer fork the SDK replaces does not creep back into the repo. The
// reference forks live in the separate spirelens/ambience repos and are out of
// scope here by construction (this walks only the glimmung module tree).
func TestSDKIsTheOnlySanctionedStepProducerSurface(t *testing.T) {
	root := repoRoot(t)
	var offenders []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor", "dist", "build":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".sh") {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if retiredShellProducer.Match(data) {
			rel, _ := filepath.Rel(root, path)
			offenders = append(offenders, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk repo: %v", err)
	}
	if len(offenders) > 0 {
		t.Fatalf("retired shell step-producer surface reintroduced in %v; use the harness/step SDK (Main/Registry/Handler) instead of a native_emit_* lib.sh fork", offenders)
	}
}

// TestSanctionedSurfaceExists is a tripwire that the sanctioned producer API is
// present and wired the way the migration guard assumes — a registry that
// dispatches a handler. If the spine is renamed or removed, this fails loudly
// so the guard above can be kept honest.
func TestSanctionedSurfaceExists(t *testing.T) {
	reg := NewRegistry().Register(HandlerFunc{StepSlug: "x", Fn: func(*Context) (Result, error) { return Result{}, nil }})
	if _, ok := reg.Handler("x"); !ok {
		t.Fatal("the sanctioned step-producer registry must dispatch a registered handler")
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not locate go.mod (repo root)")
		}
		dir = parent
	}
}
