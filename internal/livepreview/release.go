// Package livepreview implements the generic live-preview edge: a standalone
// reverse-proxy + receiver that serves a developer's freshly-built frontend
// bundle override-first in front of a stable app backend.
//
// This is the data plane of the live-preview lane (docs/features/test-slots
// contract), a separate lane from the faithful image-deploy lane: a live
// preview is scratch for *seeing* UI iterate in seconds and is never a
// validation input. The receiver core here is ported from tank-operator's
// battle-tested static-override receiver
// (backend-go/cmd/tank-operator/handlers_static_override.go) — the extract
// guards, the atomic `current` symlink flip, and the prune-to-N-releases
// retention — and is the only part reused. tank-operator's serving / toggle /
// SSE / daemon surface is NOT ported; it is the retired in-app path. The edge
// is the generic standalone replacement.
//
// It uses none of the retired hot-swap vocabulary that
// scripts/check-deleted-test-slot-hot-swap.mjs forbids; the only vocabulary
// here is live-preview / static-override.
//
// On-disk layout under the override root (the per-pod emptyDir the chart
// mounts):
//
//	<root>/releases/rel-<ts>-<rand>/dist/      extracted frontend bundle (served)
//	<root>/releases/rel-<ts>-<rand>/meta.json  build id + pushed_at (NOT served)
//	<root>/current                             symlink → releases/rel-...  (atomically flipped)
//
// A request resolves the served tree through <root>/current/dist on every
// request, so a symlink rename is a zero-window atomic swap: a request never
// observes a half-written bundle. Keeping meta.json a sibling of dist (not
// inside it) means the build-id marker travels with the atomic flip yet is
// never web-served.
package livepreview

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	// MaxPushCompressedBytes bounds the compressed request body. A production
	// frontend dist/ is a few MB gzipped; 64 MiB is generous headroom while
	// capping a runaway or hostile upload. The edge enforces it with
	// http.MaxBytesReader so the read fails fast without buffering the body.
	MaxPushCompressedBytes int64 = 64 << 20

	// maxPushUncompressedBytes bounds the total extracted size so a
	// decompression bomb cannot fill the override emptyDir.
	maxPushUncompressedBytes int64 = 256 << 20

	// maxPushEntries bounds the number of archive members.
	maxPushEntries = 20000

	// keepReleases is how many extracted bundles to retain (the live one plus
	// a little history for cheap rollback); older ones are pruned after each
	// successful flip so the emptyDir does not grow without bound.
	keepReleases = 3

	releasesDirName = "releases"
	currentName     = "current"
	releasePrefix   = "rel-"
	distSubdir      = "dist"
	metaFileName    = "meta.json"
)

// ErrNoIndex rejects an archive with no index.html at its root: such a bundle
// has no SPA entrypoint, so flipping `current` to it would serve a dead
// frontend. Failing the push leaves the prior good bundle live.
var ErrNoIndex = errors.New("archive missing index.html at root")

// ErrBadArchive classifies a client-fault push: a corrupt/oversized/unsafe
// archive. The edge maps it to HTTP 400 + push outcome "bad_archive". A
// compressed-size overflow (http.MaxBytesError) is wrapped inside this chain
// too, so the edge checks for that type first to report "too_large" instead.
var ErrBadArchive = errors.New("invalid archive")

// ErrInternal classifies a server-fault push: a filesystem error preparing,
// writing, or activating the release. The edge maps it to HTTP 500 + push
// outcome "error". `current` is never flipped when one of these fires.
var ErrInternal = errors.New("internal error")

// ReleaseMeta is the per-release marker persisted next to (not inside) the
// served bundle. It is the durable source of the LIVE build id that
// /__live-preview/status reports — the build of the bundle `current` points at,
// which is the contract Glimmung's observed-read-back verifier depends on.
type ReleaseMeta struct {
	// Build is the build id the pusher supplied (X-Live-Preview-Build), e.g. a
	// content hash or git SHA of the built frontend. It is what the read-back
	// verifier compares against the build it pushed.
	Build string `json:"build"`
	// Release is the release directory name (rel-<ts>-<rand>).
	Release string `json:"release"`
	// PushedAt is when this bundle was activated.
	PushedAt time.Time `json:"pushed_at"`
}

// Status is the JSON shape returned by GET /__live-preview/status. It reports
// the LIVE bundle — the one `current` resolves to — not merely the last push
// attempt, so a failed push (which never flips `current`) leaves status
// reporting the prior good build.
type Status struct {
	// OverrideActive is true when an override bundle is live (the `current`
	// symlink resolves to a release with a served dist/).
	OverrideActive bool `json:"override_active"`
	// Build is the LIVE build id, "" when no override is active.
	Build string `json:"build"`
	// Release is the LIVE release directory name, "" when no override is active.
	Release string `json:"release"`
	// PushedAt is the RFC3339 activation time of the live bundle, "" when no
	// override is active.
	PushedAt string `json:"pushed_at"`
}

// PushResult summarizes a successful activation.
type PushResult struct {
	Meta  ReleaseMeta
	Files int
	Bytes int64
}

// Store owns the override root directory and the release lifecycle. It is the
// pure filesystem state machine behind the edge handler; it does no HTTP, no
// auth, and no metrics so it is exhaustively unit-testable.
type Store struct {
	root string
	// maxUncompressed and maxEntries default to the package caps; tests in the
	// package lower them so the decompression-bomb and entry-flood guards can
	// be exercised cheaply instead of at 256 MiB / 20000 entries.
	maxUncompressed int64
	maxEntries      int
}

// NewStore returns a Store rooted at the override directory (the emptyDir the
// chart mounts). The directory is created lazily on first push.
func NewStore(root string) *Store {
	return &Store{
		root:            strings.TrimSpace(root),
		maxUncompressed: maxPushUncompressedBytes,
		maxEntries:      maxPushEntries,
	}
}

// Root returns the override root directory.
func (s *Store) Root() string { return s.root }

func (s *Store) releasesDir() string { return filepath.Join(s.root, releasesDirName) }
func (s *Store) currentPath() string { return filepath.Join(s.root, currentName) }

// Push extracts a gzipped tar of a built frontend dist/ into a fresh release
// directory, persists the build-id marker, and atomically activates it as the
// served override. `r` is the (already compressed-capped) request body; `build`
// is the pusher-supplied build id; `pushedAt` stamps the marker. On any
// extraction error the partial release directory is removed and `current` is
// left untouched, so a bad push never flips a live preview to a broken bundle.
func (s *Store) Push(r io.Reader, build string, pushedAt time.Time) (PushResult, error) {
	if err := os.MkdirAll(s.releasesDir(), 0o755); err != nil {
		return PushResult{}, fmt.Errorf("%w: prepare releases dir: %w", ErrInternal, err)
	}
	relDir, err := os.MkdirTemp(s.releasesDir(), fmt.Sprintf("%s%020d-*", releasePrefix, pushedAt.UnixNano()))
	if err != nil {
		return PushResult{}, fmt.Errorf("%w: create release dir: %w", ErrInternal, err)
	}

	distDir := filepath.Join(relDir, distSubdir)
	files, nbytes, err := s.extractTar(r, distDir)
	if err != nil {
		_ = os.RemoveAll(relDir)
		// ErrNoIndex stays a distinct sentinel; every other extraction fault is
		// a bad archive. The original error (including any http.MaxBytesError
		// for a compressed-size overflow) is preserved in the chain so the edge
		// can detect too_large via errors.As before falling back to bad_archive.
		if errors.Is(err, ErrNoIndex) {
			return PushResult{}, err
		}
		return PushResult{}, fmt.Errorf("%w: %w", ErrBadArchive, err)
	}

	meta := ReleaseMeta{
		Build:    build,
		Release:  filepath.Base(relDir),
		PushedAt: pushedAt.UTC(),
	}
	if err := writeMeta(filepath.Join(relDir, metaFileName), meta); err != nil {
		_ = os.RemoveAll(relDir)
		return PushResult{}, fmt.Errorf("%w: write release meta: %w", ErrInternal, err)
	}

	relTarget := filepath.Join(releasesDirName, filepath.Base(relDir))
	if err := flipCurrent(s.currentPath(), relTarget); err != nil {
		_ = os.RemoveAll(relDir)
		return PushResult{}, fmt.Errorf("%w: activate release: %w", ErrInternal, err)
	}
	pruneReleases(s.releasesDir(), s.currentPath(), keepReleases)

	return PushResult{Meta: meta, Files: files, Bytes: nbytes}, nil
}

// Delete reverts the edge to backend passthrough by removing the `current`
// symlink and dropping the extracted releases. It reports whether an override
// was active before the call so the caller can distinguish a real revert from a
// no-op DELETE.
func (s *Store) Delete() (wasActive bool, err error) {
	_, wasActive = s.ActiveDistDir()
	if rmErr := os.Remove(s.currentPath()); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
		return wasActive, fmt.Errorf("clear override: %w", rmErr)
	}
	// Drop the extracted bundles too: a reverted edge proxies to the backend,
	// so retaining releases would only leak emptyDir space.
	if rmErr := os.RemoveAll(s.releasesDir()); rmErr != nil {
		return wasActive, fmt.Errorf("clear releases: %w", rmErr)
	}
	return wasActive, nil
}

// ActiveDistDir returns the served bundle directory (<root>/current/dist) and
// true when an override is live. It is the single routing predicate the edge
// uses per request: present → serve the override, absent → proxy the backend.
func (s *Store) ActiveDistDir() (string, bool) {
	distDir := filepath.Join(s.currentPath(), distSubdir)
	info, err := os.Stat(distDir) // follows the `current` symlink
	if err != nil || !info.IsDir() {
		return "", false
	}
	return distDir, true
}

// Status resolves the LIVE bundle and returns the read-back contract shape. It
// reads the marker through the `current` symlink so it always reports the build
// `current` points at, even if a later push failed mid-flight.
func (s *Store) Status() Status {
	if _, ok := s.ActiveDistDir(); !ok {
		return Status{OverrideActive: false}
	}
	st := Status{OverrideActive: true}
	meta, err := readMeta(filepath.Join(s.currentPath(), metaFileName))
	if err != nil {
		// dist/ exists but the marker is unreadable — report active with no
		// build rather than lying about passthrough. Push writes meta before
		// flipping, so this only happens under on-disk corruption.
		return st
	}
	st.Build = meta.Build
	st.Release = meta.Release
	if !meta.PushedAt.IsZero() {
		st.PushedAt = meta.PushedAt.UTC().Format(time.RFC3339)
	}
	return st
}

// extractTar streams a gzipped tar into dest with full containment,
// entry-count, and uncompressed-size guards. It returns the number of regular
// files written and their total byte count. It rejects any member that escapes
// dest, any symlink/hardlink/device member (never extract a link from an
// untrusted archive into the served tree), and any archive with no root
// index.html. Ported from tank-operator's extractStaticOverrideTar.
func (s *Store) extractTar(r io.Reader, dest string) (int, int64, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return 0, 0, fmt.Errorf("gzip: %w", err)
	}
	defer gz.Close()

	if err := os.MkdirAll(dest, 0o755); err != nil {
		return 0, 0, err
	}
	destAbs, err := filepath.Abs(dest)
	if err != nil {
		return 0, 0, err
	}
	sep := string(filepath.Separator)

	tr := tar.NewReader(gz)
	var files int
	var total int64
	var sawIndex bool
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return files, total, fmt.Errorf("tar: %w", err)
		}
		if files >= s.maxEntries {
			return files, total, fmt.Errorf("archive exceeds %d entries", s.maxEntries)
		}

		name := filepath.Clean(strings.TrimPrefix(hdr.Name, "./"))
		if name == "." || name == "" {
			continue
		}
		if filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, ".."+sep) {
			return files, total, fmt.Errorf("unsafe path in archive: %q", hdr.Name)
		}
		target := filepath.Join(destAbs, name)
		if rel, rerr := filepath.Rel(destAbs, target); rerr != nil || rel == ".." || strings.HasPrefix(rel, ".."+sep) {
			return files, total, fmt.Errorf("path escapes destination: %q", hdr.Name)
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return files, total, err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return files, total, err
			}
			remaining := s.maxUncompressed - total
			if remaining <= 0 {
				return files, total, fmt.Errorf("archive exceeds %d uncompressed bytes", s.maxUncompressed)
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
			if err != nil {
				return files, total, err
			}
			// Copy at most remaining+1 so a member that would push us over the
			// budget is detected (n > remaining) without trusting hdr.Size.
			n, copyErr := io.CopyN(f, tr, remaining+1)
			closeErr := f.Close()
			total += n
			if copyErr != nil && !errors.Is(copyErr, io.EOF) {
				return files, total, copyErr
			}
			if n > remaining {
				return files, total, fmt.Errorf("archive exceeds %d uncompressed bytes", s.maxUncompressed)
			}
			if closeErr != nil {
				return files, total, closeErr
			}
			files++
			if name == "index.html" {
				sawIndex = true
			}
		default:
			return files, total, fmt.Errorf("unsupported tar entry type %q for %q", string(rune(hdr.Typeflag)), hdr.Name)
		}
	}
	if !sawIndex {
		return files, total, ErrNoIndex
	}
	return files, total, nil
}

// flipCurrent atomically points currentPath at relTarget (a path relative to
// the override root, e.g. "releases/rel-..."). It writes a temp symlink and
// renames it over the existing `current`; rename(2) over a symlink is atomic,
// so a concurrent request resolves either the old or the new bundle, never a
// partial state. Ported from tank-operator's flipStaticOverrideCurrent.
func flipCurrent(currentPath, relTarget string) error {
	tmpLink := fmt.Sprintf("%s.tmp.%d", currentPath, time.Now().UnixNano())
	_ = os.Remove(tmpLink)
	if err := os.Symlink(relTarget, tmpLink); err != nil {
		return err
	}
	if err := os.Rename(tmpLink, currentPath); err != nil {
		_ = os.Remove(tmpLink)
		return err
	}
	return nil
}

// pruneReleases removes all but the newest `keep` extracted bundles. Release
// dir names are time-prefixed, so a lexical sort is chronological. The bundle
// `current` points at is never pruned, even if it somehow falls outside the
// keep window. Ported from tank-operator's pruneStaticOverrideReleases.
func pruneReleases(releasesDir, currentPath string, keep int) {
	entries, err := os.ReadDir(releasesDir)
	if err != nil {
		return
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), releasePrefix) {
			names = append(names, e.Name())
		}
	}
	if len(names) <= keep {
		return
	}
	sort.Strings(names)

	var liveTarget string
	if dst, err := os.Readlink(currentPath); err == nil {
		liveTarget = filepath.Base(dst)
	}
	for _, name := range names[:len(names)-keep] {
		if name == liveTarget {
			continue
		}
		_ = os.RemoveAll(filepath.Join(releasesDir, name))
	}
}

func writeMeta(path string, meta ReleaseMeta) error {
	data, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func readMeta(path string) (ReleaseMeta, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return ReleaseMeta{}, err
	}
	var meta ReleaseMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return ReleaseMeta{}, err
	}
	return meta, nil
}
