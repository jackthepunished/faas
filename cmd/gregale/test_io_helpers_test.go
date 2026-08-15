// test_io_helpers_test.go — customer-side test I/O helpers
// (issue #911 / ADR-110 PR-6.5).
//
// swapIO + swapStdout were originally declared in cmd/gregale/sign_keys_test.go
// (the operator-side sign-keys test file). After PR-6.5 that file moved
// to cmd/gregalectl/, but customer-side tests (commands_init_test.go,
// commands_deployments_test.go) still need them. We duplicate the
// helpers here so the customer package compiles in isolation.
//
// Duplication is intentional (two binaries, two main packages, no
// shared internal package at this PR — see PR-7 follow-up).
package main

import (
	"bytes"
	"os"
	"testing"
)

// swapIO captures writes to both osStdout (the package seam) and
// os.Stderr (which printErr + PrintUsage write to directly).
// os.Stdout and os.Stderr are *os.File in Go's stdlib — the
// stderr redirect uses a real temp file rather than a *bytes.Buffer.
// Returns the in-memory stdout buffer, a function that reads the
// stderr file into a string when called (deferred so the test body
// can run before the file is read), and a single restore func.
func swapIO(t *testing.T) (stdout *bytes.Buffer, readStderr func() string, restore func()) {
	t.Helper()
	var outBuf bytes.Buffer
	errFile, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatalf("create stderr temp: %v", err)
	}
	oldOut := osStdout
	oldErr := os.Stderr
	// Also swap the package var osStderr (commands3.go) so error-path
	// output routed through printErr's PrintWarn/renderAPIError flow
	// lands in the tempfile too.
	oldPkgErr := osStderr
	osStdout = &outBuf
	os.Stderr = errFile
	osStderr = errFile
	restore = func() {
		osStdout = oldOut
		os.Stderr = oldErr
		osStderr = oldPkgErr
		_ = errFile.Close()
	}
	readStderr = func() string {
		if err := errFile.Sync(); err != nil {
			t.Logf("stderr sync: %v", err)
		}
		path := errFile.Name()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Logf("stderr read: %v", err)
			return ""
		}
		return string(data)
	}
	return &outBuf, readStderr, restore
}

// swapStdout captures writes to osStdout only.
func swapStdout(t *testing.T) (*bytes.Buffer, func()) {
	t.Helper()
	var buf bytes.Buffer
	oldOut := osStdout
	osStdout = &buf
	return &buf, func() { osStdout = oldOut }
}
