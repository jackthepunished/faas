package gitfetch

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A public repository needs no credential. Sending "Authorization: Bearer "
// with an empty token is not merely useless — GitHub treats a malformed
// credential as a failed authentication rather than an anonymous request.
func TestFetch_EmptyTokenSendsNoAuthorizationHeader(t *testing.T) {
	var sawAuthHeader bool
	var headerValue string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headerValue = r.Header.Get("Authorization")
		_, sawAuthHeader = r.Header["Authorization"]
		w.Header().Set("Content-Type", "application/x-gzip")
		_, _ = w.Write(tinyArchive(t))
	}))
	defer srv.Close()

	fetcher := NewHTTPWithBase(t.TempDir(), srv.URL, srv.Client(), 0, 0)
	tree, err := fetcher.Fetch(context.Background(), "gregale/api", "0123456789abcdef0123456789abcdef01234567", "")
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	defer func() { _ = tree.Close() }()

	if sawAuthHeader {
		t.Errorf("Authorization header sent as %q for an anonymous fetch, want none", headerValue)
	}
}

func tinyArchive(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	body := []byte(`{"name":"api"}`)
	if err := tw.WriteHeader(&tar.Header{
		Name: "gregale-api-0123456/package.json", Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatalf("tar header: %v", err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatalf("tar write: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}
