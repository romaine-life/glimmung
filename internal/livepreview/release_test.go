package livepreview

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type tarEntry struct {
	name     string
	body     string
	typeflag byte
	linkname string
}

func reg(name, body string) tarEntry { return tarEntry{name: name, body: body, typeflag: tar.TypeReg} }

// tarGz builds a gzipped tar from the entries in order.
func tarGz(t *testing.T, entries ...tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Mode: 0o644, Typeflag: e.typeflag, Linkname: e.linkname}
		switch e.typeflag {
		case tar.TypeReg:
			hdr.Size = int64(len(e.body))
		case tar.TypeDir:
			hdr.Mode = 0o755
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write header %q: %v", e.name, err)
		}
		if e.typeflag == tar.TypeReg && e.body != "" {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatalf("write body %q: %v", e.name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	return buf.Bytes()
}

// bundle returns a minimal valid bundle (index.html at root) plus any extras.
func bundle(t *testing.T, extra ...tarEntry) []byte {
	t.Helper()
	entries := append([]tarEntry{reg("index.html", "<html>app</html>")}, extra...)
	return tarGz(t, entries...)
}

func pushBytes(t *testing.T, s *Store, data []byte, build string) (PushResult, error) {
	t.Helper()
	return s.Push(bytes.NewReader(data), build, time.Now())
}

func TestStorePushActivatesAndStatusReadsBackBuild(t *testing.T) {
	s := NewStore(t.TempDir())
	data := bundle(t, reg("assets/app.js", "console.log(1)"))

	res, err := pushBytes(t, s, data, "build-abc")
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if res.Files != 2 {
		t.Errorf("files=%d, want 2", res.Files)
	}
	if res.Meta.Build != "build-abc" {
		t.Errorf("meta build=%q, want build-abc", res.Meta.Build)
	}

	distDir, ok := s.ActiveDistDir()
	if !ok {
		t.Fatal("expected an active override after push")
	}
	got, err := os.ReadFile(filepath.Join(distDir, "index.html"))
	if err != nil || string(got) != "<html>app</html>" {
		t.Fatalf("served index.html=%q err=%v", got, err)
	}

	// The read-back contract: status reports the LIVE build, release, pushed_at.
	st := s.Status()
	if !st.OverrideActive {
		t.Error("status override_active=false, want true")
	}
	if st.Build != "build-abc" {
		t.Errorf("status build=%q, want build-abc", st.Build)
	}
	if st.Release != res.Meta.Release || !strings.HasPrefix(st.Release, releasePrefix) {
		t.Errorf("status release=%q, want %q", st.Release, res.Meta.Release)
	}
	if st.PushedAt == "" {
		t.Error("status pushed_at empty, want RFC3339 timestamp")
	}
	if _, err := time.Parse(time.RFC3339, st.PushedAt); err != nil {
		t.Errorf("status pushed_at=%q not RFC3339: %v", st.PushedAt, err)
	}

	// The build marker must NOT be inside the served tree.
	if _, err := os.Stat(filepath.Join(distDir, metaFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("meta.json should not live under the served dist dir (err=%v)", err)
	}
}

func TestStorePushRejectsMissingIndex(t *testing.T) {
	s := NewStore(t.TempDir())
	data := tarGz(t, reg("app.js", "x")) // no index.html

	_, err := pushBytes(t, s, data, "b")
	if !errors.Is(err, ErrNoIndex) {
		t.Fatalf("err=%v, want ErrNoIndex", err)
	}
	if _, ok := s.ActiveDistDir(); ok {
		t.Error("a rejected push must not activate an override")
	}
	if st := s.Status(); st.OverrideActive {
		t.Error("status should report no override after a rejected push")
	}
}

func TestStorePushRejectsUnsafePaths(t *testing.T) {
	cases := map[string][]tarEntry{
		"parent escape":  {reg("index.html", "x"), reg("../escape.txt", "x")},
		"deep escape":    {reg("index.html", "x"), reg("a/../../escape.txt", "x")},
		"absolute path":  {reg("index.html", "x"), reg("/etc/passwd", "x")},
		"symlink member": {reg("index.html", "x"), {name: "link", typeflag: tar.TypeSymlink, linkname: "/etc/passwd"}},
		"hardlink":       {reg("index.html", "x"), {name: "hard", typeflag: tar.TypeLink, linkname: "index.html"}},
		"device member":  {reg("index.html", "x"), {name: "dev", typeflag: tar.TypeChar}},
	}
	for name, entries := range cases {
		t.Run(name, func(t *testing.T) {
			s := NewStore(t.TempDir())
			_, err := pushBytes(t, s, tarGz(t, entries...), "b")
			if !errors.Is(err, ErrBadArchive) {
				t.Fatalf("err=%v, want ErrBadArchive", err)
			}
			if _, ok := s.ActiveDistDir(); ok {
				t.Error("a rejected push must not activate an override")
			}
		})
	}
}

func TestStorePushRejectsTooManyEntries(t *testing.T) {
	s := NewStore(t.TempDir())
	s.maxEntries = 3
	entries := []tarEntry{reg("index.html", "x"), reg("a", "1"), reg("b", "2"), reg("c", "3")}
	_, err := pushBytes(t, s, tarGz(t, entries...), "b")
	if !errors.Is(err, ErrBadArchive) {
		t.Fatalf("err=%v, want ErrBadArchive (entry cap)", err)
	}
}

func TestStorePushRejectsUncompressedBomb(t *testing.T) {
	s := NewStore(t.TempDir())
	s.maxUncompressed = 8
	data := bundle(t, reg("big.bin", strings.Repeat("A", 64)))
	_, err := pushBytes(t, s, data, "b")
	if !errors.Is(err, ErrBadArchive) {
		t.Fatalf("err=%v, want ErrBadArchive (uncompressed cap)", err)
	}
}

func TestStorePushRejectsNonGzip(t *testing.T) {
	s := NewStore(t.TempDir())
	_, err := pushBytes(t, s, []byte("this is not gzip"), "b")
	if !errors.Is(err, ErrBadArchive) {
		t.Fatalf("err=%v, want ErrBadArchive (bad gzip)", err)
	}
}

func TestStoreSecondPushFlipsBuild(t *testing.T) {
	s := NewStore(t.TempDir())
	if _, err := pushBytes(t, s, bundle(t, reg("v", "one")), "build-1"); err != nil {
		t.Fatalf("push 1: %v", err)
	}
	if _, err := pushBytes(t, s, bundle(t, reg("v", "two")), "build-2"); err != nil {
		t.Fatalf("push 2: %v", err)
	}

	st := s.Status()
	if st.Build != "build-2" {
		t.Errorf("status build=%q, want build-2 (replace, not first-install)", st.Build)
	}
	distDir, _ := s.ActiveDistDir()
	got, _ := os.ReadFile(filepath.Join(distDir, "v"))
	if string(got) != "two" {
		t.Errorf("served v=%q, want two", got)
	}
}

func TestStoreDeleteReverts(t *testing.T) {
	s := NewStore(t.TempDir())
	if _, err := pushBytes(t, s, bundle(t), "b"); err != nil {
		t.Fatalf("push: %v", err)
	}
	wasActive, err := s.Delete()
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if !wasActive {
		t.Error("delete should report wasActive=true after a push")
	}
	if _, ok := s.ActiveDistDir(); ok {
		t.Error("override should be gone after delete")
	}
	if st := s.Status(); st.OverrideActive {
		t.Error("status should report no override after delete")
	}
	// The releases dir is dropped so the emptyDir doesn't leak.
	if _, err := os.Stat(s.releasesDir()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("releases dir should be removed on delete (err=%v)", err)
	}
}

func TestStoreDeleteWithNoOverrideIsNoOp(t *testing.T) {
	s := NewStore(t.TempDir())
	wasActive, err := s.Delete()
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if wasActive {
		t.Error("delete on a fresh store should report wasActive=false")
	}
}

func TestStoreFailedPushKeepsPreviousLive(t *testing.T) {
	s := NewStore(t.TempDir())
	if _, err := pushBytes(t, s, bundle(t, reg("v", "good")), "build-good"); err != nil {
		t.Fatalf("push good: %v", err)
	}

	// A bad push (no index.html) must not flip the live bundle.
	if _, err := pushBytes(t, s, tarGz(t, reg("v", "bad")), "build-bad"); !errors.Is(err, ErrNoIndex) {
		t.Fatalf("bad push err=%v, want ErrNoIndex", err)
	}

	st := s.Status()
	if st.Build != "build-good" {
		t.Errorf("status build=%q, want build-good (failed push must not flip)", st.Build)
	}
	distDir, _ := s.ActiveDistDir()
	got, _ := os.ReadFile(filepath.Join(distDir, "v"))
	if string(got) != "good" {
		t.Errorf("served v=%q, want good", got)
	}
}

func TestStorePrunesToKeepReleases(t *testing.T) {
	s := NewStore(t.TempDir())
	for i := 0; i < keepReleases+3; i++ {
		// Distinct nanosecond timestamps keep release dir names ordered.
		if _, err := s.Push(bytes.NewReader(bundle(t)), "b", time.Now().Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatalf("push %d: %v", i, err)
		}
	}
	entries, err := os.ReadDir(s.releasesDir())
	if err != nil {
		t.Fatalf("read releases: %v", err)
	}
	n := 0
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), releasePrefix) {
			n++
		}
	}
	if n > keepReleases {
		t.Errorf("retained %d releases, want <= %d", n, keepReleases)
	}
	if _, ok := s.ActiveDistDir(); !ok {
		t.Error("current must still resolve after pruning")
	}
}

func TestResolveServePath(t *testing.T) {
	dist := "/srv/dist"
	cases := []struct {
		url      string
		wantRel  string
		wantOK   bool
	}{
		{"/", "index.html", true},
		{"/index.html", "index.html", true},
		{"/assets/app.js", "assets/app.js", true},
		{"/../../etc/passwd", "etc/passwd", true}, // path.Clean neutralizes the escape
		{"/a/../b", "b", true},
	}
	for _, c := range cases {
		got, ok := resolveServePath(dist, c.url)
		if ok != c.wantOK {
			t.Errorf("%s: ok=%v, want %v", c.url, ok, c.wantOK)
			continue
		}
		want := filepath.Join(dist, filepath.FromSlash(c.wantRel))
		if got != want {
			t.Errorf("%s: got %q, want %q", c.url, got, want)
		}
		if !strings.HasPrefix(got, dist) {
			t.Errorf("%s: resolved path %q escaped dist %q", c.url, got, dist)
		}
	}
}
