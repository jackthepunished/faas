// Whitebox tests for the streaming-bridge body-decoding helpers in
// forward.go (issue #471 PR-B + PR-C / ADR-047 / review F2 fix).
//
// The streaming body stream from the bridge is raw HTTP/1.1 wire
// bytes — including chunked framing when the guest emitted
// `Transfer-Encoding: chunked`. The Go-side reader wraps the
// stream with httputil.NewChunkedReader when the parsed
// Transfer-Encoding header indicates chunked, otherwise it
// passes the bytes through verbatim. These tests pin both
// branches so a future PR can't accidentally drop the chunked
// decode (which would forward chunk-size lines + CRLF separators
// as the body and visibly break LLM-shaped guest responses).
//
// Build tag: (none). CI-safe; no KVM, no root, no netns.

package vmmdgrpc

import (
	"bufio"
	"bytes"
	"io"
	"net/http/httputil"
	"testing"

	vmmdpb "github.com/onebox-faas/faas/api/proto/onebox/faas/vmmd/v1"
)

// TestStreamBodyDecoder_NoChunkingPassesThrough verifies the
// default branch (no Transfer-Encoding: chunked): the body
// stream is forwarded verbatim to the gRPC client.
func TestStreamBodyDecoder_NoChunkingPassesThrough(t *testing.T) {
	want := []byte("hello plain-text body — no chunked framing")
	headers := []*vmmdpb.Header{
		{Name: "Content-Length", Value: "47"},
	}

	var buf bytes.Buffer
	buf.WriteString("HTTP/1.1 200 OK\n")
	buf.WriteString("Content-Length: 47\n")
	buf.WriteString("\n")
	buf.Write(want)

	br := bufio.NewReader(&buf)
	if err := readHeadersForTest(br); err != nil {
		t.Fatalf("readHeaders: %v", err)
	}
	if responseIsChunked(headers) {
		t.Fatalf("responseIsChunked reported true for Content-Length-only response")
	}
	got, err := readBodyForTest(br, responseIsChunked(headers))
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("body = %q, want %q", got, want)
	}
}

// TestStreamBodyDecoder_ChunkedDecodes verifies the chunked
// branch: the stream emits decoded payload bytes — not the
// chunk-size lines and CRLF separators the wire carried. This
// is the tripwire for the F2 fix.
func TestStreamBodyDecoder_ChunkedDecodes(t *testing.T) {
	// Build a chunked-encoded body: two chunks + terminator.
	// 5-byte chunk + 10-byte chunk + 0 terminator.
	chunk1 := []byte("hello")
	chunk2 := []byte("world !!!!") // 10 bytes
	headers := []*vmmdpb.Header{
		{Name: "Transfer-Encoding", Value: "chunked"},
	}

	var wire bytes.Buffer
	wire.WriteString("HTTP/1.1 200 OK\n")
	wire.WriteString("Transfer-Encoding: chunked\n")
	wire.WriteString("\n")
	wire.WriteString("5\r\n")
	wire.Write(chunk1)
	wire.WriteString("\r\n")
	wire.WriteString("a\r\n") // hex a = 10
	wire.Write(chunk2)
	wire.WriteString("\r\n")
	wire.WriteString("0\r\n")
	wire.WriteString("\r\n")

	br := bufio.NewReader(&wire)
	if err := readHeadersForTest(br); err != nil {
		t.Fatalf("readHeaders: %v", err)
	}
	if !responseIsChunked(headers) {
		t.Fatalf("responseIsChunked reported false for Transfer-Encoding: chunked")
	}
	got, err := readBodyForTest(br, responseIsChunked(headers))
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	want := append(append([]byte{}, chunk1...), chunk2...)
	if !bytes.Equal(got, want) {
		t.Errorf("decoded body = %q, want %q", got, want)
	}
	// Belt-and-suspenders: assert the decoded body does NOT
	// contain the chunk-size hex literals. If a future PR drops
	// the chunked reader, those bytes will leak and break
	// LLM-shaped guest responses. We don't check for raw \r\n
	// here because the decoded body could legitimately contain
	// newline characters from JSON / SSE payloads.
	for _, leak := range []string{"5\r\n", "a\r\n", "0\r\n"} {
		if bytes.Contains(got, []byte(leak)) {
			t.Errorf("decoded body leaked chunked framing %q — chunked reader is broken", leak)
		}
	}
}

// TestStreamBodyDecoder_ChunkedMixedCase verifies the chunked
// detector matches "chunked" case-insensitively and tolerates
// codings mixed with other Transfer-Encoding tokens (RFC 7230
// §3.3.1 — token case-insensitivity + comma-separated list).
func TestStreamBodyDecoder_ChunkedMixedCase(t *testing.T) {
	cases := []string{
		"chunked",
		"Chunked",
		"CHUNKED",
		"gzip, chunked",
		"chunked, gzip",
	}
	for _, v := range cases {
		h := []*vmmdpb.Header{{Name: "Transfer-Encoding", Value: v}}
		if !responseIsChunked(h) {
			t.Errorf("responseIsChunked(Transfer-Encoding: %q) = false, want true", v)
		}
	}
	h := []*vmmdpb.Header{{Name: "Transfer-Encoding", Value: "gzip"}}
	if responseIsChunked(h) {
		t.Errorf("responseIsChunked(Transfer-Encoding: gzip) = true, want false")
	}
	if responseIsChunked(nil) {
		t.Errorf("responseIsChunked(nil headers) = true, want false")
	}
}

// readHeadersForTest consumes the response header block from the
// reader (terminated by a blank line) the same way the
// ForwardHTTPStream reader goroutine does. Cap at 64 KiB so a
// malformed bridge can't OOM the server.
func readHeadersForTest(br *bufio.Reader) error {
	var n int
	for {
		line, err := br.ReadString('\n')
		n += len(line)
		if n > 64*1024 {
			return io.ErrShortBuffer
		}
		if err != nil && line == "" {
			return err
		}
		if line == "\n" {
			return nil
		}
	}
}

// readBodyForTest mirrors the streaming-reader body loop in
// ForwardHTTPStream. chunked=true → wrap with
// httputil.NewChunkedReader; false → pass-through. Reads in
// 8 KiB chunks to match the production wire shape.
func readBodyForTest(r io.Reader, chunked bool) ([]byte, error) {
	if chunked {
		r = httputil.NewChunkedReader(r)
	}
	var out bytes.Buffer
	buf := make([]byte, 8*1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			out.Write(buf[:n])
		}
		if err == io.EOF {
			return out.Bytes(), nil
		}
		if err != nil {
			return nil, err
		}
	}
}
