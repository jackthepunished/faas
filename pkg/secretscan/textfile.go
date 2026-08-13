// Text-file detection for the secret scanner.
//
// IsTextFile decides whether a file's contents (or the prefix the caller
// has in hand) are text-shaped enough to scan. Without this gate, every
// PNG, JPEG, wasm blob, compiled object, archive, font, etc. in a customer
// tarball would be regex-matched against base64-shaped substrings. The
// false-positive rate erodes trust in the scanner and is the difference
// between "useful" and "noise that customers mute".
//
// The check is two-stage, ordered cheapest-first:
//
//  1. NUL-byte probe over the first 8 KiB. A NUL byte is a strong signal
//     of binary content because the secret-pattern regexes assume printable
//     ASCII (the provider tokens are A-Z / a-z / 0-9 / base64 + the PEM
//     armour delimiters `-----BEGIN ... PRIVATE KEY-----`). git uses the
//     same heuristic.
//
//  2. http.DetectContentType as a safety net for archives and compressed
//     blobs (gz/bz2/xz/PDF/Zip) that have no NULs but obvious signatures.
//     `strings.HasPrefix(contentType, "text/")` is the accept criterion;
//     anything else (image/*, audio/*, video/*, application/octet-stream,
//     application/pdf, application/zip, application/x-tar,
//     application/x-gzip) is rejected.
//
// The function is total: empty head returns false (no information to
// judge). JSON, YAML, TOML, .env, Python, TypeScript, Go, Rust, etc. all
// pass the NUL-byte check and are detected as text/plain or
// application/json (which IsTextFile treats as text via the JSON-prefix
// accept below).
package secretscan

import (
	"bytes"
	"io"
	"net/http"
	"strings"
)

// textFileHeadBytes is the size of the prefix read for the NUL probe +
// MIME detection. 8 KiB matches git's default x-check-attr binary
// heuristic and is enough for http.DetectContentType to fingerprint any
// realistic archive or image header.
const textFileHeadBytes = 8 << 10

// textFileMimeHeadBytes is the prefix passed to http.DetectContentType.
// The stdlib reads only the first 512 bytes; passing more is fine (it
// reads what it needs) but we cap at textFileHeadBytes to avoid copying
// the whole probe buffer a second time.
const textFileMimeHeadBytes = 512

// IsTextFile reports whether head (a prefix of the file's contents) is a
// text file. Pass nil or empty for an empty file (returns false because
// there is no information to judge). The path argument is currently unused
// but reserved for extension-based bypasses (e.g. .txt, .env); see the
// call site in cmd/apid/secretscan.go for the path-aware shape.
func IsTextFile(path string, head []byte) bool {
	_ = path // reserved for future extension-aware bypass
	if len(head) == 0 {
		return false
	}
	// Stage 1: NUL-byte probe. Cheap, deterministic.
	if bytes.IndexByte(head, 0) >= 0 {
		return false
	}
	// Stage 2: MIME sniff. Reads at most 512 bytes.
	probe := head
	if len(probe) > textFileMimeHeadBytes {
		probe = probe[:textFileMimeHeadBytes]
	}
	contentType := http.DetectContentType(probe)
	if strings.HasPrefix(contentType, "text/") {
		return true
	}
	// application/json is JSON text; secret patterns can appear in JSON
	// values, so accept it.
	if strings.HasPrefix(contentType, "application/json") {
		return true
	}
	return false
}

// ReadHead reads up to textFileHeadBytes from r and returns it. It is a
// helper for the common caller pattern (open file → read up to N → feed
// into IsTextFile → decide whether to read the rest). It does NOT close r;
// callers use a `defer f.Close()`.
func ReadHead(r io.Reader) ([]byte, error) {
	buf := make([]byte, textFileHeadBytes)
	n, err := io.ReadFull(r, buf)
	if err == io.EOF || err == io.ErrUnexpectedEOF {
		// File is smaller than the cap; that's fine.
		return buf[:n], nil
	}
	if err != nil {
		return nil, err
	}
	return buf[:n], nil
}
