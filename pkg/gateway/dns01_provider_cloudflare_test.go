// Cloudflare DNS-01 solver tests (PR-8).
//
// These run against an httptest.Server stub that mimics the Cloudflare
// API v4 JSON surface (zones list + dns_records create + dns_records
// delete). No outbound network — same shape as dns01_provider_hetzner_test.go.
package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/libdns/libdns"
)

type fakeCloudflare struct {
	server *httptest.Server
	mux    *http.ServeMux
	zones  map[string]string // zone name → id
	recs   map[string]bool   // record id → exists
}

func newFakeCloudflare(t *testing.T) *fakeCloudflare {
	t.Helper()
	f := &fakeCloudflare{
		mux:   http.NewServeMux(),
		zones: map[string]string{"example.com": "zoneid-123"},
		recs:  map[string]bool{},
	}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/zones") && r.Method == http.MethodGet:
			zone := r.URL.Query().Get("name")
			id, ok := f.zones[zone]
			if !ok {
				_ = json.NewEncoder(w).Encode(cloudflareZonesResp{Success: true, Result: nil})
				return
			}
			_ = json.NewEncoder(w).Encode(cloudflareZonesResp{
				Success: true,
				Result: []struct {
					ID   string `json:"id"`
					Name string `json:"name"`
				}{{ID: id, Name: zone}},
			})
		case strings.Contains(r.URL.Path, "/dns_records") && r.Method == http.MethodPost:
			id := "recid-456"
			f.recs[id] = true
			_ = json.NewEncoder(w).Encode(cloudflareRecordResp{Success: true, Result: struct {
				ID string `json:"id"`
			}{ID: id}})
		case strings.Contains(r.URL.Path, "/dns_records/") && r.Method == http.MethodDelete:
			parts := strings.Split(r.URL.Path, "/")
			id := parts[len(parts)-1]
			delete(f.recs, id)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(f.server.Close)
	return f
}

func TestCloudflareDNSProvider_AppendRecords(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFakeCloudflare(t)

	p := newCloudflareDNSProviderForTest(
		"test-token", "example.com",
		f.server.URL, f.server.Client(),
	)

	recs := []libdns.Record{
		libdns.TXT{
			Name: "_acme-challenge",
			TTL:  60,
			Text: "challenge-token-xyz",
		},
	}
	out, err := p.AppendRecords(ctx, "example.com", recs)
	if err != nil {
		t.Fatalf("AppendRecords: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("expected 1 echoed record, got %d", len(out))
	}
	got, ok := out[0].(libdns.TXT)
	if !ok {
		t.Fatalf("expected libdns.TXT, got %T", out[0])
	}
	if got.Text != "challenge-token-xyz" {
		t.Errorf("Text = %q, want challenge-token-xyz", got.Text)
	}
	if got.ProviderData != "recid-456" {
		t.Errorf("ProviderData = %v, want recid-456", got.ProviderData)
	}
}

func TestCloudflareDNSProvider_AppendRecords_ZoneNotFound(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFakeCloudflare(t)
	p := newCloudflareDNSProviderForTest(
		"test-token", "example.com",
		f.server.URL, f.server.Client(),
	)
	_, err := p.AppendRecords(ctx, "missing.com", []libdns.Record{
		libdns.TXT{Name: "_acme-challenge", Text: "x"},
	})
	if err == nil || !strings.Contains(err.Error(), "zone") {
		t.Errorf("expected zone-not-found error, got %v", err)
	}
}

func TestCloudflareDNSProvider_AppendRecords_RejectsNonTXT(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFakeCloudflare(t)
	p := newCloudflareDNSProviderForTest(
		"test-token", "example.com",
		f.server.URL, f.server.Client(),
	)
	_, err := p.AppendRecords(ctx, "example.com", []libdns.Record{
		// libdns.A wraps an A-record (Address); the solver rejects non-TXT.
		libdns.Address{Name: "x", IP: netip.MustParseAddr("1.2.3.4")},
	})
	if err == nil || !strings.Contains(err.Error(), "unsupported record type") {
		t.Errorf("expected unsupported-record-type error, got %v", err)
	}
}

func TestCloudflareDNSProvider_DeleteRecords(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFakeCloudflare(t)
	p := newCloudflareDNSProviderForTest(
		"test-token", "example.com",
		f.server.URL, f.server.Client(),
	)

	// First create a record so DeleteRecords has a valid ID to remove.
	out, err := p.AppendRecords(ctx, "example.com", []libdns.Record{
		libdns.TXT{Name: "_acme-challenge", Text: "x"},
	})
	if err != nil {
		t.Fatalf("AppendRecords setup: %v", err)
	}
	if _, err := p.DeleteRecords(ctx, "example.com", out); err != nil {
		t.Fatalf("DeleteRecords: %v", err)
	}
	if f.recs["recid-456"] {
		t.Errorf("record id recid-456 should be deleted")
	}
}

func TestCloudflareDNSProvider_DeleteRecords_NoProviderDataSkipped(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFakeCloudflare(t)
	p := newCloudflareDNSProviderForTest(
		"test-token", "example.com",
		f.server.URL, f.server.Client(),
	)
	// A libdns.TXT with no ProviderData (never appended) must be skipped
	// silently — best-effort cleanup matches libdns convention.
	_, err := p.DeleteRecords(ctx, "example.com", []libdns.Record{
		libdns.TXT{Name: "_acme-challenge", Text: "x"},
	})
	if err != nil {
		t.Errorf("DeleteRecords with no ProviderData: unexpected err: %v", err)
	}
}

func TestCloudflareDNSProvider_AuthHeader(t *testing.T) {
	t.Parallel()
	// Capture the auth header the provider sends. Cloudflare expects
	// "Authorization: Bearer <token>" — NOT a custom auth-API-Token header
	// like Hetzner.
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		// Return a zone list with the requested zone present so zoneID()
		// doesn't short-circuit on "zone not found".
		_, _ = io.Copy(io.Discard, r.Body)
		_ = json.NewEncoder(w).Encode(cloudflareZonesResp{
			Success: true,
			Result: []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			}{{ID: "zoneid-xyz", Name: "example.com"}},
		})
	}))
	defer srv.Close()

	p := newCloudflareDNSProviderForTest("my-token", "example.com", srv.URL, srv.Client())
	if _, err := p.zoneID(context.Background(), "example.com"); err != nil {
		t.Fatalf("zoneID: %v", err)
	}
	if got != "Bearer my-token" {
		t.Errorf("Authorization = %q, want %q", got, "Bearer my-token")
	}
}

// (no netParseIP helper — use net.ParseIP directly in tests)
