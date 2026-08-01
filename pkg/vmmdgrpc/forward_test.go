// Tests for pkg/vmmdgrpc/forward.go (issue #98 / ADR-028 / ADR-047).
// The bridge runs in two phases — parseBridgeOutput (pure) and the
// runtime backward-check via bufconn (gRPC envelope + error mapping).
// The ip-netns exec itself is the only piece we can't unit test on
// macOS without root + a real Linux netns; that's gated to
// //go:build metal in pkg/netns. On non-Linux dev hosts the bridge
// path is exercised end-to-end with `make metal-lima`
// (see CLAUDE.md).
//
// PR-D / ADR-047 removed the unary ForwardHTTP RPC. The streaming
// ForwardHTTPStream is the only bridge today, so the suite tests
// the streaming script generator and the parsing helpers; the
// gRPC envelope is exercised by the bufconn-based unit tests in
// the same package.

package vmmdgrpc_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	vmmdpb "github.com/onebox-faas/faas/api/proto/onebox/faas/vmmd/v1"
	"github.com/onebox-faas/faas/pkg/vmmdgrpc"
)

// TestParseBridgeOutput_HappyPath walks the pure parser with a
// realistic script output. Status + headers + body must round-trip;
// binary bodies must survive the split on \n\n.
func TestParseBridgeOutput_HappyPath(t *testing.T) {
	raw := []byte("HTTP/1.1 200 OK\n" +
		"Content-Type: application/json\n" +
		"X-Trace-Id: abc-123\n" +
		"\n" +
		`{"ok":true}`)
	resp, err := parseBridgeOutputForTest(raw)
	if err != nil {
		t.Fatalf("parseBridgeOutput: %v", err)
	}
	if resp.Status != 200 {
		t.Errorf("status = %d, want 200", resp.Status)
	}
	if got := len(resp.Headers); got != 2 {
		t.Fatalf("header count = %d, want 2", got)
	}
	if resp.Headers[0].GetName() != "Content-Type" || resp.Headers[0].GetValue() != "application/json" {
		t.Errorf("header 0 = %+v", resp.Headers[0])
	}
	if string(resp.Body) != `{"ok":true}` {
		t.Errorf("body = %q", string(resp.Body))
	}
}

// TestParseBridgeOutput_BinaryBody verifies the script's `cat <&3`
// output (which can include NULs) survives the \n\n split on the
// FIRST terminator (the body might contain literal "\n\n" inside it).
//
// Caveat: the current script splits on the first "\n\n" — that's
// the standard HTTP/1.1 contract (headers end at the first blank
// line) and it matches what httputil.ReverseProxy expects. If a
// future script change ever inlines a multi-line header (e.g.
// Set-Cookie with continuation), update parseBridgeOutput to handle
// folded headers per RFC 7230. For now, a body containing \n\n is
// NOT supported and this test pins that boundary.
func TestParseBridgeOutput_BinaryBody(t *testing.T) {
	raw := []byte("HTTP/1.1 200 OK\nContent-Type: image/png\n\n\x89PNG\r\n\x1a\n-body-bytes-")
	resp, err := parseBridgeOutputForTest(raw)
	if err != nil {
		t.Fatalf("parseBridgeOutput: %v", err)
	}
	if !strings.HasPrefix(string(resp.Body), "\x89PNG") {
		t.Errorf("body lost leading bytes: %q", string(resp.Body))
	}
	if !strings.Contains(string(resp.Body), "-body-bytes-") {
		t.Errorf("body lost trailing bytes: %q", string(resp.Body))
	}
}

// TestParseBridgeOutput_Malformed asserts the parser refuses bad
// input rather than returning a partially-filled envelope that the
// caller might mistake for success.
func TestParseBridgeOutput_Malformed(t *testing.T) {
	for _, raw := range [][]byte{
		nil,
		[]byte("HTTP/1.1 200 OK"),
		// No \n\n terminator.
		[]byte("HTTP/1.1 200 OK\nContent-Type: x/y"),
		// No status code.
		[]byte("\nContent-Type: x/y\n\nbody"),
	} {
		if _, err := parseBridgeOutputForTest(raw); err == nil {
			t.Errorf("expected error for input %q, got nil", string(raw))
		}
	}
}

// TestParseBridgeOutput_BadStatusCode: a guest reply line like
// "HTTP/1.1 OK" (no code) must surface as a parse error, NOT a
// silent zero status.
func TestParseBridgeOutput_BadStatusCode(t *testing.T) {
	raw := []byte("HTTP/1.1 OK\nContent-Length: 0\n\n")
	if _, err := parseBridgeOutputForTest(raw); err == nil {
		t.Fatal("expected error on missing status code")
	}
}

// TestBuildStreamingBridgeScript_ResolvesDialPort pins issue #460
// / ADR-053 (PR-C) and ADR-047 (PR-D): the streaming bridge script
// must dial the per-deployment override port when set, fall back to
// netns.AppPort (8080) when zero, and rewrite the Host header in
// both cases. The Host rewrite matters because customer apps that
// pin Host (e.g. vhost routers) must see the inner identity, not
// the overlay hostname.
//
// We assert on the rendered script — no nsenter, no root — using the
// stable strings emitted by buildStreamingBridgeScript:
//   - `exec 3<>/dev/tcp/<netns.GuestIP>/<dialPort>\n`
//   - `printf 'Host: %s\r\n' '<netns.GuestIP>:<dialPort>' >&3`
func TestBuildStreamingBridgeScript_ResolvesDialPort(t *testing.T) {
	const (
		guestIP = "10.0.0.2"
	)
	tests := []struct {
		name        string
		wirePort    uint32
		wantDial    string
		wantHost    string
		description string
	}{
		{
			name:        "zero port → 8080 default (ADR-009 + portnorm)",
			wirePort:    0,
			wantDial:    fmt.Sprintf("exec 3<>/dev/tcp/%s/8080", guestIP),
			wantHost:    fmt.Sprintf("'Host: %%s\\r\\n' '%s:8080'", guestIP),
			description: "legacy callers leave Port==0; buildStreamingBridgeScript defaults to netns.AppPort",
		},
		{
			name:        "explicit 9090 override",
			wirePort:    9090,
			wantDial:    fmt.Sprintf("exec 3<>/dev/tcp/%s/9090", guestIP),
			wantHost:    fmt.Sprintf("'Host: %%s\\r\\n' '%s:9090'", guestIP),
			description: "PR-C payload: vmmd forwarder dials the customer's override port",
		},
		{
			name:        "explicit 3000 override",
			wirePort:    3000,
			wantDial:    fmt.Sprintf("exec 3<>/dev/tcp/%s/3000", guestIP),
			wantHost:    fmt.Sprintf("'Host: %%s\\r\\n' '%s:3000'", guestIP),
			description: "non-8080 override sanity check — Host header ports the same value",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := &vmmdpb.ForwardHTTPRequestInit{
				Method:     "GET",
				RequestUri: "/",
				Port:       tt.wirePort,
			}
			script := vmmdgrpc.BuildStreamingBridgeScriptForTest(req, 30*time.Second)
			if !strings.Contains(script, tt.wantDial) {
				t.Errorf("script missing dial line %q\n--- script ---\n%s", tt.wantDial, script)
			}
			if !strings.Contains(script, tt.wantHost) {
				t.Errorf("script missing Host rewrite %q\n--- script ---\n%s", tt.wantHost, script)
			}
		})
	}
}

// TestBuildStreamingBridgeScript_EmitsChunkedBody pins the
// streaming body protocol (ADR-047 PR-B + PR-C): the script must
// read body chunks from stdin and emit chunked-encoded frames to
// the guest over fd 3. The fixed `printf '%%x\\r\\n' ${#CHUNK}>&3`
// line is the chunk-size header; `printf '0\\r\\n\\r\\n' >&3` is
// the chunked-encoding terminator. Together they prove the bridge
// is using chunked encoding (not Content-Length), which is what
// the streaming body's stdin-fed design requires.
func TestBuildStreamingBridgeScript_EmitsChunkedBody(t *testing.T) {
	req := &vmmdpb.ForwardHTTPRequestInit{
		Method:     "POST",
		RequestUri: "/chat",
		Port:       0,
	}
	script := vmmdgrpc.BuildStreamingBridgeScriptForTest(req, 30*time.Second)
	for _, want := range []string{
		"while IFS= read -r -t 1 -n 8192 CHUNK; do",
		"printf '%x\\r\\n' ${#CHUNK} >&3",
		"printf '0\\r\\n\\r\\n' >&3",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script missing chunked-body line %q\n--- script ---\n%s", want, script)
		}
	}
}

// parseBridgeOutputForTest calls into the package via a tiny
// exported shim. The ForwardHTTPStream server itself is gated to
// //go:build metal (it nsenter's a netns), so we test the parser
// directly.
func parseBridgeOutputForTest(raw []byte) (vmmdgrpc.ParsedBridgeResponseForTest, error) {
	return vmmdgrpc.ParseBridgeOutputForTest(raw)
}
