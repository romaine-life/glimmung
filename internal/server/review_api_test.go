package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/romaine-life/glimmung/internal/auth"
)

type fakeReviewStore struct {
	fakeReadStore
	rows   []ReviewRow
	detail ReviewDetail
	err    error
}

func (s *fakeReviewStore) ListReviews(_ context.Context, _ ReviewListFilter) ([]ReviewRow, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.rows, nil
}

func (s *fakeReviewStore) GetReviewForIssue(_ context.Context, _ string, _ int) (ReviewDetail, error) {
	if s.err != nil {
		return ReviewDetail{}, s.err
	}
	return s.detail, nil
}

func (s *fakeReviewStore) EnsureReview(_ context.Context, req ReviewCreate) (ReviewDetail, error) {
	if s.err != nil {
		return ReviewDetail{}, s.err
	}
	return ReviewDetail{
		Ref:      req.Repo + "#" + itoa(req.Number),
		Project:  req.Project,
		Repo:     req.Repo,
		PRNumber: req.Number,
		Title:    req.Title,
		State:    "ready",
	}, nil
}

func itoa(n int) string {
	return strings.TrimSpace(strings.Replace(" "+string(rune('0'+n)), " ", "", -1))
}

func TestListReviews(t *testing.T) {
	store := &fakeReviewStore{rows: []ReviewRow{{
		Ref:      "romaine-life/glimmung#42",
		Project:  "glimmung",
		Repo:     "romaine-life/glimmung",
		PRNumber: 42,
		Title:    "Fix dashboard",
		State:    "ready",
	}}}
	handler := NewWithStore(Settings{}, store)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/reviews?project=glimmung", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"ref":"romaine-life/glimmung#42"`) {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

func TestIssueReviewDetail(t *testing.T) {
	store := &fakeReviewStore{detail: ReviewDetail{
		Ref: "romaine-life/glimmung#42", Project: "glimmung", Repo: "romaine-life/glimmung", PRNumber: 42, Title: "Fix dashboard", State: "ready",
	}}
	handler := NewWithStore(Settings{}, store)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/projects/glimmung/issues/17/review", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestCreateReview(t *testing.T) {
	store := &fakeReviewStore{}
	handler := NewWithDependencies(Settings{}, store, fakeAdminAuthenticator{user: auth.User{Sub: "admin", Email: "admin@example.com"}})

	rec := httptest.NewRecorder()
	body := `{"project":"glimmung","repo":"romaine-life/glimmung","number":42,"title":"Fix","branch":"fix-branch"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/reviews", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer admin")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"title":"Fix"`) {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

func TestCreateReviewValidates(t *testing.T) {
	handler := NewWithDependencies(Settings{}, &fakeReviewStore{}, fakeAdminAuthenticator{user: auth.User{Sub: "admin"}})

	cases := []struct {
		body string
		desc string
	}{
		{`{"repo":"romaine-life/glimmung","number":1,"title":"t","branch":"b"}`, "missing project"},
		{`{"project":"glimmung","number":1,"title":"t","branch":"b"}`, "missing repo"},
		{`{"project":"glimmung","repo":"romaine-life/glimmung","title":"t","branch":"b"}`, "missing number"},
		{`{"project":"glimmung","repo":"romaine-life/glimmung","number":1,"branch":"b"}`, "missing title"},
		{`{"project":"glimmung","repo":"romaine-life/glimmung","number":1,"title":"t"}`, "missing branch"},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/reviews", strings.NewReader(tc.body))
		req.Header.Set("Authorization", "Bearer admin")
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: status=%d body=%s", tc.desc, rec.Code, rec.Body.String())
		}
	}
}

func TestReviewRequiresStore(t *testing.T) {
	handler := NewWithStore(Settings{}, fakeReadStore{})
	for _, path := range []string{"/v1/reviews", "/v1/projects/glimmung/issues/1/review"} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("path=%s status=%d body=%s", path, rec.Code, rec.Body.String())
		}
	}
}
