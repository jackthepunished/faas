// changedfiles_test.go — tests for the GitHub compare-API client
// (ADR-050 §103-109). Pins the truncation detection, the
// retry-on-429 budget, and the standard outbound headers.
package githubd

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// fixedClient returns a TokenCache backed by a stub fetcher and a
// singleHostClient that routes requests to the supplied test
// server. The token returned by the stub is constant so the
// Authorization-header test can assert the bearer prefix verbatim.
func fixedClient(t *testing.T, srv *httptest.Server, token string) (ChangedFilesClient, *TokenCache) {
	t.Helper()
	fetcher := fakeFetcher(func(ctx context.Context, id int64) (string, time.Time, error) {
		return token, time.Now().Add(time.Hour), nil
	})
	tc := NewTokenCache(fetcher, time.Minute)
	hc := &singleHostClient{base: srv.Client(), api: srv.URL}
	return NewHTTPChangedFiles(tc, hc), tc
}

func TestChangedFiles_Basic(t *testing.T) {
	t.Parallel()
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"status":"ahead",
			"ahead_by":1,
			"behind_by":0,
			"total_commits":1,
			"commits":[{"sha":"c1"}],
			"files":[
				{"filename":"services/auth/api/index.ts","status":"modified"},
				{"filename":"services/auth/api/Dockerfile","status":"modified"},
				{"filename":"README.md","status":"modified"}
			]
		}`))
	}))
	defer srv.Close()

	client, _ := fixedClient(t, srv, "tok")
	got, err := client.ChangedFiles(context.Background(), 7, "octo", "api", "base-sha", "head-sha")
	if err != nil {
		t.Fatalf("ChangedFiles: %v", err)
	}
	want := []string{
		"services/auth/api/index.ts",
		"services/auth/api/Dockerfile",
		"README.md",
	}
	if !stringSliceEq(got, want) {
		t.Errorf("files = %v, want %v", got, want)
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Errorf("calls = %d, want 1", calls)
	}
}

func TestChangedFiles_RenamedFileIncludesBothPaths(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{
			"status":"ahead","ahead_by":1,"behind_by":0,
			"total_commits":1,"commits":[{"sha":"c1"}],
			"files":[
				{"filename":"services/auth/api/x.ts","previous_filename":"services/auth/y.ts","status":"renamed"}
			]
		}`))
	}))
	defer srv.Close()

	client, _ := fixedClient(t, srv, "tok")
	got, err := client.ChangedFiles(context.Background(), 7, "octo", "api", "base-sha", "head-sha")
	if err != nil {
		t.Fatalf("ChangedFiles: %v", err)
	}
	want := []string{"services/auth/api/x.ts", "services/auth/y.ts"}
	if !stringSliceEq(got, want) {
		t.Errorf("files = %v, want %v", got, want)
	}
}

func TestChangedFiles_RemovedFileIncluded(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{
			"status":"ahead","ahead_by":1,"behind_by":0,
			"total_commits":1,"commits":[{"sha":"c1"}],
			"files":[
				{"filename":"services/auth/api/deprecated.go","status":"removed"}
			]
		}`))
	}))
	defer srv.Close()

	client, _ := fixedClient(t, srv, "tok")
	got, err := client.ChangedFiles(context.Background(), 7, "octo", "api", "base-sha", "head-sha")
	if err != nil {
		t.Fatalf("ChangedFiles: %v", err)
	}
	if len(got) != 1 || got[0] != "services/auth/api/deprecated.go" {
		t.Errorf("files = %v, want [services/auth/api/deprecated.go]", got)
	}
}

func TestChangedFiles_TruncatedByCommitsPagination(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 250 total commits but only 2 in the page → diff is too
		// large to trust per ADR-050 §109.
		_, _ = w.Write([]byte(`{
			"status":"ahead","ahead_by":250,"behind_by":0,
			"total_commits":250,
			"commits":[{"sha":"c1"},{"sha":"c2"}],
			"files":[{"filename":"x","status":"modified"}]
		}`))
	}))
	defer srv.Close()

	client, _ := fixedClient(t, srv, "tok")
	_, err := client.ChangedFiles(context.Background(), 7, "octo", "api", "base-sha", "head-sha")
	if !errors.Is(err, ErrTruncated) {
		t.Errorf("err = %v, want ErrTruncated", err)
	}
}

func TestChangedFiles_TruncatedByFiles300Cap(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := []byte(`{"status":"ahead","ahead_by":1,"behind_by":0,"total_commits":1,"commits":[{"sha":"c1"}],"files":[`)
		for i := 0; i < compareFilesCap; i++ {
			if i > 0 {
				body = append(body, ',')
			}
			body = append(body, []byte(`{"filename":"f`+strconv.Itoa(i)+`","status":"modified"}`)...)
		}
		body = append(body, []byte(`]}`)...)
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	client, _ := fixedClient(t, srv, "tok")
	_, err := client.ChangedFiles(context.Background(), 7, "octo", "api", "base-sha", "head-sha")
	if !errors.Is(err, ErrTruncated) {
		t.Errorf("err = %v, want ErrTruncated", err)
	}
}

func TestChangedFiles_RetryOn429(t *testing.T) {
	t.Parallel()
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(`{
			"status":"ahead","ahead_by":1,"behind_by":0,
			"total_commits":1,"commits":[{"sha":"c1"}],
			"files":[{"filename":"a","status":"modified"}]
		}`))
	}))
	defer srv.Close()

	client, _ := fixedClient(t, srv, "tok")
	got, err := client.ChangedFiles(context.Background(), 7, "octo", "api", "base-sha", "head-sha")
	if err != nil {
		t.Fatalf("ChangedFiles: %v", err)
	}
	if len(got) != 1 || got[0] != "a" {
		t.Errorf("files = %v, want [a]", got)
	}
	if atomic.LoadInt32(&calls) != 2 {
		t.Errorf("calls = %d, want 2 (initial + 1 retry)", calls)
	}
}

func TestChangedFiles_RetryExhaustedMapsToTruncated(t *testing.T) {
	t.Parallel()
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	client, _ := fixedClient(t, srv, "tok")
	_, err := client.ChangedFiles(context.Background(), 7, "octo", "api", "base-sha", "head-sha")
	if !errors.Is(err, ErrTruncated) {
		t.Errorf("err = %v, want ErrTruncated (429-exhausted is semantically truncation)", err)
	}
	// 1 initial + 2 retries = 3 attempts.
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("calls = %d, want 3", got)
	}
}

func TestChangedFiles_404ReturnsUnavailable(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	client, _ := fixedClient(t, srv, "tok")
	_, err := client.ChangedFiles(context.Background(), 7, "octo", "api", "base-sha", "head-sha")
	if err == nil {
		t.Fatal("err = nil, want non-nil")
	}
	// 404 is not truncation; it's a clean "unavailable" so the
	// caller can decide whether to surface a different audit row.
	if errors.Is(err, ErrTruncated) {
		t.Errorf("err = ErrTruncated, want wrapped ErrUnavailable")
	}
}

func TestChangedFiles_StandardHeaders(t *testing.T) {
	t.Parallel()
	var seenAuth, seenAccept, seenVersion, seenUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		seenAccept = r.Header.Get("Accept")
		seenVersion = r.Header.Get("X-GitHub-Api-Version")
		seenUA = r.Header.Get("User-Agent")
		_, _ = w.Write([]byte(`{"status":"ahead","ahead_by":0,"behind_by":0,"total_commits":0,"commits":[],"files":[]}`))
	}))
	defer srv.Close()

	client, _ := fixedClient(t, srv, "tok-xyz")
	_, _ = client.ChangedFiles(context.Background(), 7, "octo", "api", "b", "h")

	if seenAuth != "Bearer tok-xyz" {
		t.Errorf("Authorization = %q, want %q", seenAuth, "Bearer tok-xyz")
	}
	if seenAccept != "application/vnd.github+json" {
		t.Errorf("Accept = %q, want application/vnd.github+json", seenAccept)
	}
	if seenVersion != "2022-11-28" {
		t.Errorf("X-GitHub-Api-Version = %q, want 2022-11-28", seenVersion)
	}
	if seenUA != "faas-githubd/1.0" {
		t.Errorf("User-Agent = %q, want faas-githubd/1.0", seenUA)
	}
}

func TestChangedFiles_EmptyBaseTruncated(t *testing.T) {
	t.Parallel()
	// Empty base can't form a compare URL — map to truncation
	// so the caller falls back to full fan-out.
	client, _ := fixedClient(t, httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("upstream should not be called for empty base")
	})), "tok")
	_, err := client.ChangedFiles(context.Background(), 7, "octo", "api", "", "h")
	if !errors.Is(err, ErrTruncated) {
		t.Errorf("err = %v, want ErrTruncated", err)
	}
}

func TestChangedFiles_EmptyOwnerRepoUnavailable(t *testing.T) {
	t.Parallel()
	client, _ := fixedClient(t, httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("upstream should not be called for empty owner/repo")
	})), "tok")
	_, err := client.ChangedFiles(context.Background(), 7, "", "api", "b", "h")
	if err == nil || errors.Is(err, ErrTruncated) {
		t.Errorf("err = %v, want wrapped ErrUnavailable", err)
	}
}

// stringSliceEq compares two unordered []string values element
// by element. Order is significant (compare preserves order);
// duplicates are allowed.
func stringSliceEq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
