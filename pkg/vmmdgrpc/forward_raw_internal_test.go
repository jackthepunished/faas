package vmmdgrpc

import (
	"bufio"
	"errors"
	"io"
	"strings"
	"testing"
)

// readUntilBlankLineTestCase is a table-driven harness for the
// head-split helper. Mirrors the legacy ForwardHTTPStream head-
// read pattern (forward.go:294-313) and pins its boundaries:
//   - terminator line `\n` is dropped (output ends at the previous
//     line's `\n`)
//   - per-line trailing `\n` is also dropped, matching the shell
//     bridge's `read -r` output shape (forward.go:493-499)
//   - 64 KiB cap returns a non-EOF error
//   - EOF without terminator returns partial bytes + io.EOF
//   - empty input returns io.EOF promptly (no busy loop)
func TestReadUntilBlankLine(t *testing.T) {
	for _, tc := range []struct {
		name      string
		input     string
		wantHead  string
		wantRest  string
		wantErrIs error // nil = success
	}{
		{
			name: "happy_path_ws_upgrade",
			input: "HTTP/1.1 101 Switching Protocols\n" +
				"Upgrade: websocket\n" +
				"Connection: Upgrade\n" +
				"Sec-WebSocket-Accept: s3pPLMBiTxaQ9kYGzzhZRbK+xOo=\n" +
				"\n",
			wantHead: "HTTP/1.1 101 Switching Protocols\n" +
				"Upgrade: websocket\n" +
				"Connection: Upgrade\n" +
				"Sec-WebSocket-Accept: s3pPLMBiTxaQ9kYGzzhZRbK+xOo=\n",
			wantRest: "",
		},
		{
			name:     "body_in_same_read",
			input:    "HTTP/1.1 200 OK\nContent-Length: 5\n\nhello",
			wantHead: "HTTP/1.1 200 OK\nContent-Length: 5\n",
			wantRest: "hello",
		},
		{
			name:      "eof_without_terminator",
			input:     "HTTP/1.1 200 OK",
			wantHead:  "HTTP/1.1 200 OK",
			wantErrIs: io.EOF,
		},
		{
			name:      "empty_input",
			input:     "",
			wantHead:  "",
			wantErrIs: io.EOF,
		},
		{
			name:      "head_exceeds_64kib",
			input:     "X-Filler: " + strings.Repeat("A", 65*1024) + "\n\n",
			wantHead:  "",
			wantErrIs: nil, // custom string-typed error, see below
			// The terminator '\n' remains on the reader because
			// readUntilBlankLine returns before consuming it.
			wantRest: "\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := bufio.NewReader(strings.NewReader(tc.input))
			got, err := readUntilBlankLine(r)
			if tc.wantErrIs != nil {
				if !errors.Is(err, tc.wantErrIs) {
					t.Fatalf("err = %v, want %v", err, tc.wantErrIs)
				}
			} else if tc.name == "head_exceeds_64kib" {
				if err == nil {
					t.Fatalf("expected cap error, got nil (head=%q)", string(got))
				}
				if !strings.Contains(err.Error(), "exceeds") {
					t.Fatalf("cap error text unexpected: %v", err)
				}
			} else if err != nil {
				t.Fatalf("readUntilBlankLine: %v", err)
			}
			if string(got) != tc.wantHead {
				t.Errorf("head = %q, want %q", string(got), tc.wantHead)
			}
			rest, _ := io.ReadAll(r)
			if string(rest) != tc.wantRest {
				t.Errorf("rest = %q, want %q", string(rest), tc.wantRest)
			}
		})
	}
}

// TestReadUntilBlankLine_DoesNotConsumeBodyBytes pins the
// subtle invariant that the body bytes that arrive in the same
// read as the head remain on the bufio.Reader for the caller to
// drain — readUntilBlankLine MUST NOT advance past the head
// terminator. parseBridgeOutput + the body loop depend on this.
func TestReadUntilBlankLine_DoesNotConsumeBodyBytes(t *testing.T) {
	input := "HTTP/1.1 200 OK\nTransfer-Encoding: chunked\n\n5\r\nhello\r\n0\r\n\r\n"
	r := bufio.NewReader(strings.NewReader(input))
	head, err := readUntilBlankLine(r)
	if err != nil {
		t.Fatalf("readUntilBlankLine: %v", err)
	}
	if !strings.HasPrefix(string(head), "HTTP/1.1 200 OK") {
		t.Errorf("head status line lost: %q", string(head))
	}
	rest, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("readAll: %v", err)
	}
	if string(rest) != "5\r\nhello\r\n0\r\n\r\n" {
		t.Errorf("body bytes mangled: %q", string(rest))
	}
}
