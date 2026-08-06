package main

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestReadResponseHead_HappyPath pins the byte-exact head split
// the bridge uses. The HTTP/1.1 response head terminator is "\n\n"
// (RFC 7230 §3); readResponseHead must return the head UP TO BUT
// NOT INCLUDING the terminator and any body bytes that arrived in
// the same read.
func TestReadResponseHead_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Test", "ok")
		w.Header().Set("Content-Length", "10")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("bodybytes"))
	}))
	defer srv.Close()

	conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Issue a real HTTP/1.1 request with a Content-Length so the
	// server emits a matching response.
	if _, err := conn.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n")); err != nil {
		t.Fatalf("write request: %v", err)
	}

	// Use a single bufio.Reader for both the head read and the
	// body read after — readResponseHead leaves any post-head
	// bytes in the bufio.Reader's internal buffer.
	breader := bufio.NewReader(conn)
	head, initialBody, err := readResponseHead(breader)
	if err != nil {
		t.Fatalf("readResponseHead: %v", err)
	}
	// Drain the rest of the body via the same bufio reader so
	// any bytes still buffered after the head are visible.
	rest, err := io.ReadAll(breader)
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("read body: %v", err)
	}
	body := append(initialBody, rest...)
	// Head must NOT contain the "\n\n" terminator.
	if bytes.Contains(head, []byte("\n\n")) {
		t.Errorf("head contains terminator: %q", string(head))
	}
	// Head must begin with the status line.
	if !strings.HasPrefix(string(head), "HTTP/1.1 200 OK") {
		t.Errorf("head status line malformed: %q", string(head))
	}
	if !bytes.Contains(head, []byte("X-Test: ok")) {
		t.Errorf("missing X-Test header in head: %q", string(head))
	}
	// Body bytes should round-trip (the test pins the head/body
	// split).
	if !bytes.Contains(body, []byte("bodybytes")) {
		t.Errorf("body bytes lost in head split: %q", string(body))
	}
}

// TestReadResponseHead_BodyAcrossMultipleReads covers the case
// where the response head and body are split across multiple
// TCP reads. readResponseHead must buffer head bytes internally
// and only return once the \n\n terminator is found.
func TestReadResponseHead_BodyAcrossMultipleReads(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Multi", "yes")
		w.Header().Set("Content-Length", "8192")
		w.WriteHeader(http.StatusOK)
		// Long body to force multiple TCP segments on the wire.
		_, _ = w.Write(bytes.Repeat([]byte("Z"), 8192))
	}))
	defer srv.Close()

	conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	if _, err := conn.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n")); err != nil {
		t.Fatalf("write: %v", err)
	}

	breader := bufio.NewReader(conn)
	head, initialBody, err := readResponseHead(breader)
	if err != nil {
		t.Fatalf("readResponseHead: %v", err)
	}
	if !strings.HasPrefix(string(head), "HTTP/1.1 200 OK") {
		t.Errorf("head status line malformed: %q", string(head))
	}
	if !bytes.Contains(head, []byte("X-Multi: yes")) {
		t.Errorf("missing X-Multi header in head: %q", string(head))
	}
	// The body prefix returned by readResponseHead plus what we
	// drain via the same bufio.Reader should match what the server
	// sent (8192 Z's).
	rest, err := io.ReadAll(breader)
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("read body: %v", err)
	}
	body := append(initialBody, rest...)
	if len(body) != 8192 {
		t.Errorf("body length = %d, want 8192", len(body))
	}
	for i, b := range body {
		if b != 'Z' {
			t.Errorf("body[%d] = %q, want 'Z'", i, b)
			break
		}
	}
}

// TestIndexDoubleLF pins the byte-exact search the bridge uses
// for the HTTP/1.1 head terminator. Faster than bytes.Index for
// the const "\n\n" but the difference is academic; the test pins
// the boundary so a future refactor can't shift behaviour.
func TestIndexDoubleLF(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want int
	}{
		{"empty", "", -1},
		{"single newline", "foo\n", -1},
		{"first \n\n at start", "\n\nrest", 0},
		{"first \n\n mid", "abc\n\ndef", 3},
		{"multiple \n\n returns first", "\n\nx\n\ny", 0},
		{"triple \n (not double)", "\n\n\n", 0},
	} {
		got := indexDoubleLF([]byte(tc.in))
		if got != tc.want {
			t.Errorf("%s: indexDoubleLF(%q) = %d, want %d", tc.name, tc.in, got, tc.want)
		}
	}
}

// TestBridge_UsageError pins the bridge's argv validation. Bad
// IP/port must exit with code 2 (usage).
func TestBridge_UsageError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("vmmd-raw-bridge uses net.JoinHostPort; skipping on Windows")
	}
	tmp := t.TempDir()
	binPath := filepath.Join(tmp, "vmmd-raw-bridge")
	if err := buildBridgeForTest(binPath); err != nil {
		t.Fatalf("build bridge: %v", err)
	}
	for _, argv := range [][]string{{}, {"only-one-arg"}, {"bad-ip", "99999"}} {
		cmd := exec.Command(binPath, argv...)
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Errorf("argv=%v: expected exit error, got %q", argv, string(out))
		}
		// Bridge exits 2 on usage errors.
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() != 2 {
			t.Errorf("argv=%v: exit code %d, want 2", argv, exitErr.ExitCode())
		}
	}
}

// TestBridge_DialRefused pins the bridge's exit code on a
// refused dial. Bind a listener, capture its port, close the
// listener, and verify the bridge exits 3 within the dial
// timeout.
func TestBridge_DialRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("vmmd-raw-bridge uses net.JoinHostPort; skipping on Windows")
	}
	tmp := t.TempDir()
	binPath := filepath.Join(tmp, "vmmd-raw-bridge")
	if err := buildBridgeForTest(binPath); err != nil {
		t.Fatalf("build bridge: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	cmd := exec.Command(binPath, host, port)
	if err := cmd.Start(); err != nil {
		t.Fatalf("bridge start: %v", err)
	}
	// 30 s dial timeout (the bridge's hard cap) — exit 3
	// should land well before that.
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() != 3 {
			t.Errorf("bridge exit: %v, want exit code 3", err)
		}
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("bridge did not exit on dial refused")
	}
}

// buildBridgeForTest compiles the vmmd-raw-bridge binary to the
// given path. Uses the standard `go build` from the cmd's parent
// dir so the test suite doesn't depend on a pre-installed binary.
func buildBridgeForTest(out string) error {
	_, src, _, ok := runtime.Caller(0)
	if !ok {
		src = "."
	}
	srcDir := filepath.Dir(src)
	cmd := exec.Command("go", "build", "-o", out, srcDir)
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
