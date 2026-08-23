// session_key_loader_test.go — mirror of cmd/apid/session_key_loader_test.go.
// See that file for the rationale (issue #585 / ADR-127 review-fix R1+R2
// — FAAS_SESSION_KEY shape contract). The gatewayd-internal loader is
// intentionally byte-equivalent to the apid one so a regression on either
// side gets caught at unit-test time rather than at production 503 time.

package main

import (
	"encoding/hex"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testHexKey = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"

func newTestLogger2() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

func TestGatewaydSessionManager_PATH_Shape(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "session.key")
	if err := os.WriteFile(keyPath, []byte(testHexKey+"\n"), 0o400); err != nil {
		t.Fatalf("write key file: %v", err)
	}
	getenv := func(name string) string {
		if name == "FAAS_SESSION_KEY" {
			return keyPath
		}
		return ""
	}
	mgr := loadSessionManager(getenv, newTestLogger2())
	if mgr == nil {
		t.Fatal("path-shaped: want manager, got nil")
	}
}

func TestGatewaydSessionManager_CONTENT_Shape(t *testing.T) {
	getenv := func(name string) string {
		if name == "FAAS_SESSION_KEY" {
			return testHexKey
		}
		return ""
	}
	mgr := loadSessionManager(getenv, newTestLogger2())
	if mgr == nil {
		t.Fatal("content-shaped: want manager, got nil")
	}
}

func TestGatewaydSessionManager_Empty_FallsBackEphemeral(t *testing.T) {
	getenv := func(string) string { return "" }
	mgr := loadSessionManager(getenv, newTestLogger2())
	if mgr == nil {
		t.Fatal("empty contract: want ephemeral manager, got nil")
	}
	// Verify the warning path was logged — use a custom capture
	// because the gatewayd loader logs WARN before returning the
	// ephemeral manager.
	buf := &strings.Builder{}
	log := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	_ = loadSessionManager(func(string) string { return "" }, log)
	if !strings.Contains(buf.String(), "ephemeral session key in use") {
		t.Errorf("empty contract: want ephemeral warning log, got %q", buf.String())
	}
}

func TestGatewaydSessionManager_WrongByteLength_FailsClosed(t *testing.T) {
	shortHex := hex.EncodeToString([]byte("0123456789abcdef"))
	getenv := func(name string) string {
		if name == "FAAS_SESSION_KEY" {
			return shortHex
		}
		return ""
	}
	mgr := loadSessionManager(getenv, newTestLogger2())
	if mgr != nil {
		t.Errorf("short-hex: want nil manager, got %+v", mgr)
	}
}
