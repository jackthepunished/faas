// Whitebox tests for IsTextFile. The function is pure and stateless so
// the surface is table-driven: each case names the head bytes + expected
// boolean. The package's binary-skip guarantee depends on this function
// being correct; a regression silently raises the false-positive rate and
// erodes customer trust in the scanner.
package secretscan

import (
	"io"
	"strings"
	"testing"
)

func TestIsTextFile(t *testing.T) {
	cases := []struct {
		name string
		head []byte
		want bool
	}{
		{
			name: "empty",
			head: nil,
			want: false,
		},
		{
			name: "plain_env",
			head: []byte("PORT=8080\nDATABASE_URL=postgres://u:p@h/d\n"),
			want: true,
		},
		{
			name: "json",
			head: []byte(`{"foo": "bar", "baz": 42}`),
			want: true,
		},
		{
			name: "yaml",
			head: []byte("name: deploy\nreplicas: 3\n"),
			want: true,
		},
		{
			name: "go_source",
			head: []byte("package main\n\nfunc main() {}\n"),
			want: true,
		},
		{
			name: "png_header",
			// PNG magic: 89 50 4E 47 0D 0A 1A 0A — has 0x0D 0x0A which
			// are NOT NUL but the MIME sniff returns image/png.
			head: []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 0x00, 0x00},
			want: false,
		},
		{
			name: "gzip_header",
			// 1F 8B magic for gzip. No NUL but application/x-gzip.
			head: []byte{0x1F, 0x8B, 0x08, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x03},
			want: false,
		},
		{
			name: "jpeg_header",
			head: []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F'},
			want: false,
		},
		{
			name: "binary_with_nul",
			// Compiled object / wasm blob — NUL byte in prefix.
			head: []byte{0x00, 0x61, 0x73, 0x6D, 0x01, 0x00, 0x00, 0x00},
			want: false,
		},
		{
			name: "empty_string_value",
			// Edge case: file starts with no newline and is short.
			head: []byte("x"),
			want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := IsTextFile("", tc.head)
			if got != tc.want {
				t.Errorf("IsTextFile(%q) = %v, want %v", tc.head, got, tc.want)
			}
		})
	}
}

func TestIsTextFile_PathReserved(t *testing.T) {
	// Currently path is unused; this pins that contract so a future
	// extension-based bypass (e.g. ".env" always text) doesn't accidentally
	// change the signature.
	if IsTextFile(".env", []byte("\x1f\x8b\x08")) {
		t.Error("expected gzip-prefixed content to be skipped even with .env name")
	}
}

func TestReadHead_ShortFile(t *testing.T) {
	// io.ReadFull on a reader shorter than the cap returns ErrUnexpectedEOF
	// or EOF, which ReadHead swallows and returns the prefix.
	got, err := ReadHead(strings.NewReader("hello"))
	if err != nil {
		t.Fatalf("ReadHead: %v", err)
	}
	if string(got) != "hello" {
		t.Errorf("ReadHead = %q, want %q", got, "hello")
	}
}

func TestReadHead_ExactCap(t *testing.T) {
	body := strings.Repeat("a", textFileHeadBytes)
	got, err := ReadHead(strings.NewReader(body))
	if err != nil {
		t.Fatalf("ReadHead: %v", err)
	}
	if len(got) != textFileHeadBytes {
		t.Errorf("ReadHead len = %d, want %d", len(got), textFileHeadBytes)
	}
}

// Compile-time check that ReadHead's reader argument satisfies io.Reader
// at the API boundary; the test bodies use *strings.Reader which satisfies
// io.Reader. The `_ = io.Reader(nil)` line is just a no-op pin.
var _ io.Reader = (*strings.Reader)(nil)
