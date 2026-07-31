package storage

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestParseLocalPrefixes_Default: an empty env var returns the
// canonical ADR-054 default list. The default is the union of
// {snap, base, kernel, layers} — content-addressed, latency-
// sensitive, small enough to keep on every box.
func TestParseLocalPrefixes_Default(t *testing.T) {
	t.Setenv("FAAS_STORAGE_LOCAL_PREFIXES", "")
	got, err := parseLocalPrefixes("")
	if err != nil {
		t.Fatalf("parseLocalPrefixes: %v", err)
	}
	want := []string{"snap/", "base/", "kernel/", "layers/"}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d (got %v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestParseLocalPrefixes_CustomList: a non-empty comma-separated
// list is honoured verbatim (whitespace + trailing slash normalised).
func TestParseLocalPrefixes_CustomList(t *testing.T) {
	got, err := parseLocalPrefixes("snap/, base/ ,kernel/")
	if err != nil {
		t.Fatalf("parseLocalPrefixes: %v", err)
	}
	want := []string{"snap/", "base/", "kernel/"}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d (got %v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestParseLocalPrefixes_Dedup: a list with duplicates dedups
// while preserving order. The router requires unique prefixes.
func TestParseLocalPrefixes_Dedup(t *testing.T) {
	got, err := parseLocalPrefixes("snap/,base/,snap/")
	if err != nil {
		t.Fatalf("parseLocalPrefixes: %v", err)
	}
	want := []string{"snap/", "base/"}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d (got %v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestParseLocalPrefixes_TrailingSlashAuto: a list with bare
// prefixes (no trailing slash) is normalised to the router's
// contract.
func TestParseLocalPrefixes_TrailingSlashAuto(t *testing.T) {
	got, err := parseLocalPrefixes("snap,base")
	if err != nil {
		t.Fatalf("parseLocalPrefixes: %v", err)
	}
	want := []string{"snap/", "base/"}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d (got %v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestParseLocalPrefixes_AllEmpty: a list of only commas +
// whitespace is rejected (the router would lose its fallback).
func TestParseLocalPrefixes_AllEmpty(t *testing.T) {
	if _, err := parseLocalPrefixes(" , , "); err == nil {
		t.Fatal("parseLocalPrefixes accepted all-empty list; want error")
	}
}

// TestBackendFromEnv_LocalRespectsCustomPrefixes: when
// FAAS_STORAGE_LOCAL_PREFIXES is set, the router carries those
// prefixes in addition to the apps prefix (no overlap with apps).
func TestBackendFromEnv_LocalRespectsCustomPrefixes(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FAAS_STORAGE_BACKEND", "local")
	t.Setenv("FAAS_STORAGE_ROOT", filepath.Join(tmp, "fc"))
	t.Setenv("FAAS_APPS_ROOT", filepath.Join(tmp, "apps"))
	t.Setenv("FAAS_STORAGE_LOCAL_PREFIXES", "snap/,base/,kernel/")
	be, err := BackendFromEnv()
	if err != nil {
		t.Fatalf("BackendFromEnv: %v", err)
	}
	router, ok := be.(*PrefixRouter)
	if !ok {
		t.Fatalf("backend type = %T, want *PrefixRouter", be)
	}
	// apps/ + snap/ + base/ + kernel/ = 4 routes; the
	// layers/ default is suppressed by the override.
	if len(router.routes) != 4 {
		t.Errorf("routes = %d, want 4 (apps + snap + base + kernel); got %v",
			len(router.routes), router.routes)
	}
}

// TestBackendFromEnv_LocalEmptyPrefixesRejected: an override
// that parses to zero prefixes is rejected at startup.
func TestBackendFromEnv_LocalEmptyPrefixesRejected(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FAAS_STORAGE_BACKEND", "local")
	t.Setenv("FAAS_STORAGE_ROOT", filepath.Join(tmp, "fc"))
	t.Setenv("FAAS_APPS_ROOT", filepath.Join(tmp, "apps"))
	t.Setenv("FAAS_STORAGE_LOCAL_PREFIXES", " , , ")
	_, err := BackendFromEnv()
	if err == nil {
		t.Fatal("BackendFromEnv accepted empty prefixes list; want error")
	}
}

// TestBackendFromEnv_LocalDefaults exercises the local backend fork
// with production default roots — the FAAS_APPS_ROOT default
// (/var/lib/faas/apps) differs from FAAS_STORAGE_ROOT (/srv/fc), so
// the helper produces a PrefixRouter.
func TestBackendFromEnv_LocalDefaults(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FAAS_STORAGE_BACKEND", "")
	t.Setenv("FAAS_STORAGE_ROOT", filepath.Join(tmp, "fc"))
	t.Setenv("FAAS_APPS_ROOT", filepath.Join(tmp, "apps"))
	be, err := BackendFromEnv()
	if err != nil {
		t.Fatalf("BackendFromEnv: %v", err)
	}
	if _, ok := be.(*PrefixRouter); !ok {
		t.Errorf("backend type = %T, want *PrefixRouter (default split)", be)
	}
}

// TestBackendFromEnv_LocalSplit exercises the local fork with
// FAAS_APPS_ROOT pointing at a separate dir (production deploys the
// two as siblings).
func TestBackendFromEnv_LocalSplit(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FAAS_STORAGE_BACKEND", "local")
	t.Setenv("FAAS_STORAGE_ROOT", filepath.Join(tmp, "fc"))
	t.Setenv("FAAS_APPS_ROOT", filepath.Join(tmp, "apps"))
	be, err := BackendFromEnv()
	if err != nil {
		t.Fatalf("BackendFromEnv: %v", err)
	}
	if _, ok := be.(*PrefixRouter); !ok {
		t.Errorf("backend type = %T, want *PrefixRouter (split layout)", be)
	}
}

// TestBackendFromEnv_LocalCoalesced verifies the router collapses to a
// single LocalStorageBackend when the two roots coincide.
func TestBackendFromEnv_LocalCoalesced(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("FAAS_STORAGE_BACKEND", "local")
	t.Setenv("FAAS_STORAGE_ROOT", tmp)
	t.Setenv("FAAS_APPS_ROOT", tmp)
	be, err := BackendFromEnv()
	if err != nil {
		t.Fatalf("BackendFromEnv: %v", err)
	}
	if _, ok := be.(*LocalStorageBackend); !ok {
		t.Errorf("backend type = %T, want *LocalStorageBackend (coalesced)", be)
	}
}

// TestBackendFromEnv_OCIRequiresRegistry verifies the OCI fork
// refuses to default without FAAS_OCI_REGISTRY.
func TestBackendFromEnv_OCIRequiresRegistry(t *testing.T) {
	t.Setenv("FAAS_STORAGE_BACKEND", "oci")
	os.Unsetenv("FAAS_OCI_REGISTRY") // ensure unset
	_, err := BackendFromEnv()
	if err == nil {
		t.Fatal("expected error for oci backend without registry")
	}
}

// TestBackendFromEnv_OCIRejectsUnknown verifies unknown backend kinds
// are rejected at startup.
func TestBackendFromEnv_OCIRejectsUnknown(t *testing.T) {
	t.Setenv("FAAS_STORAGE_BACKEND", "s3")
	_, err := BackendFromEnv()
	if err == nil {
		t.Fatal("expected error for unknown backend kind")
	}
	if got := err.Error(); !strings.Contains(got, "unknown") {
		t.Errorf("error %q lacks 'unknown'", got)
	}
}
