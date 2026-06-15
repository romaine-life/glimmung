package server

import (
	"context"
	"errors"
	"image"
	"image/color"
	"net/http"
	"net/http/httptest"
	"testing"
)

type fakeArtifactStore struct {
	artifact  Artifact
	err       error
	downloads []string
}

func (s *fakeArtifactStore) Download(_ context.Context, blobName string) (Artifact, error) {
	s.downloads = append(s.downloads, blobName)
	if s.err != nil {
		return Artifact{}, s.err
	}
	return s.artifact, nil
}

func TestReadArtifactServesScopedBlob(t *testing.T) {
	store := &fakeArtifactStore{artifact: Artifact{Body: []byte("png-bytes"), ContentType: "image/png"}}
	handler := NewWithDependencies(Settings{}, nil, nil, store)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/artifacts/runs/glimmung/01RUN/home.png", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "png-bytes" {
		t.Fatalf("body=%q", rec.Body.String())
	}
	if rec.Header().Get("content-type") != "image/png" {
		t.Fatalf("content-type=%q", rec.Header().Get("content-type"))
	}
	if rec.Header().Get("cache-control") != "public, max-age=300" {
		t.Fatalf("cache-control=%q", rec.Header().Get("cache-control"))
	}
	if len(store.downloads) != 1 || store.downloads[0] != "runs/glimmung/01RUN/home.png" {
		t.Fatalf("downloads=%#v", store.downloads)
	}
}

func TestReadArtifactRejectsUnscopedAndDotDotPaths(t *testing.T) {
	store := &fakeArtifactStore{}
	handler := NewWithDependencies(Settings{}, nil, nil, store)
	for _, path := range []string{
		"/v1/artifacts/private/secret.png",
		"/v1/artifacts/runs/glimmung/../secret.png",
		"/v1/artifacts/inspections/../escape.png",
	} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("path=%s status=%d body=%s", path, rec.Code, rec.Body.String())
		}
	}
	if len(store.downloads) != 0 {
		t.Fatalf("downloads=%#v", store.downloads)
	}
}

func TestReadArtifactMapsStoreErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{name: "missing", err: ErrArtifactNotFound, want: http.StatusNotFound},
		{name: "generic", err: errors.New("boom"), want: http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler := NewWithDependencies(Settings{}, nil, nil, &fakeArtifactStore{err: tc.err})
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/artifacts/runs/glimmung/a.txt", nil))
			if rec.Code != tc.want {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestReadArtifactServesInspectionPrefix(t *testing.T) {
	store := &fakeArtifactStore{artifact: Artifact{Body: []byte("inspection-png"), ContentType: "image/png"}}
	handler := NewWithDependencies(Settings{}, nil, nil, store)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/artifacts/inspections/lease-1/insp-1/screenshot.png", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "inspection-png" {
		t.Fatalf("body=%q", rec.Body.String())
	}
	if len(store.downloads) != 1 || store.downloads[0] != "inspections/lease-1/insp-1/screenshot.png" {
		t.Fatalf("downloads=%#v", store.downloads)
	}
}

func TestReadArtifactRefusesBlankVideo(t *testing.T) {
	orig := firstFrameExtractor
	t.Cleanup(func() { firstFrameExtractor = orig })

	// A confirmed blank first frame must never reach a human, even though this
	// video is served straight from the blob and never crossed the
	// review-promotion gate — exactly the flashbang path from run 7.1.
	firstFrameExtractor = func(context.Context, []byte) (image.Image, error) {
		return solidFrame(48, 48, color.RGBA{255, 255, 255, 255}), nil
	}
	store := &fakeArtifactStore{artifact: Artifact{Body: []byte("webm-bytes"), ContentType: "video/webm"}}
	handler := NewWithDependencies(Settings{}, nil, nil, store)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/artifacts/runs/glimmung/01RUN/videos/effect.webm", nil))

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("blank video must be refused; status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() == "webm-bytes" {
		t.Fatal("blank video bytes must not be served to a human")
	}
}

func TestReadArtifactServesPaintedVideo(t *testing.T) {
	orig := firstFrameExtractor
	t.Cleanup(func() { firstFrameExtractor = orig })

	firstFrameExtractor = func(context.Context, []byte) (image.Image, error) {
		return contentFrame(48, 48), nil
	}
	store := &fakeArtifactStore{artifact: Artifact{Body: []byte("webm-bytes"), ContentType: "video/webm"}}
	handler := NewWithDependencies(Settings{}, nil, nil, store)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/artifacts/runs/glimmung/01RUN/videos/effect.webm", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("painted video must be served; status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "webm-bytes" {
		t.Fatalf("body=%q", rec.Body.String())
	}
	if rec.Header().Get("content-type") != "video/webm" {
		t.Fatalf("content-type=%q", rec.Header().Get("content-type"))
	}
}

func TestReadArtifactBlankGateIsVideoOnly(t *testing.T) {
	// A PNG screenshot must serve unconditionally — the gate is video-only and
	// must not run ffmpeg on an image, even one that would read as uniform.
	orig := firstFrameExtractor
	t.Cleanup(func() { firstFrameExtractor = orig })
	called := false
	firstFrameExtractor = func(context.Context, []byte) (image.Image, error) {
		called = true
		return solidFrame(8, 8, color.RGBA{255, 255, 255, 255}), nil
	}
	store := &fakeArtifactStore{artifact: Artifact{Body: []byte("png-bytes"), ContentType: "image/png"}}
	handler := NewWithDependencies(Settings{}, nil, nil, store)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/artifacts/runs/glimmung/01RUN/screenshots/home.png", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("screenshot must serve; status=%d body=%s", rec.Code, rec.Body.String())
	}
	if called {
		t.Fatal("blank-frame extractor must not run on a non-video artifact")
	}
}

func TestReadArtifactRequiresStore(t *testing.T) {
	handler := NewWithDependencies(Settings{}, nil, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/artifacts/runs/glimmung/a.txt", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}
