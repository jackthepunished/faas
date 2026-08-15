// Deterministic JSON encoding helpers for the daemon_hashes map.
// Ordered iteration via manifest.SortedHostKeys() is the load-bearing
// invariant: the byte-level JSON must match across rebuilds of the
// same git_sha, otherwise doctor (PR-4) hash comparisons produce
// false-positive drift.
package releaseinstall

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/onebox-faas/faas/pkg/manifest"
)

// EncodeDaemonHashes marshals the daemon_hashes map to JSON in
// manifest.SortedHostKeys() order. This is the canonical encoding
// used by store.go for INSERT/UPDATE into release_bundles.daemon_hashes.
//
// The output is alphabetically sorted by daemon name (the natural
// order of SortedHostKeys) and is byte-stable across rebuilds of
// the same release. Mirrors pkg/manifest/pkg_manifest encoding
// discipline for the per-deploy concept.
//
// The map MUST contain an entry for every daemon in the catalog;
// Build() guarantees this. Encoding a partial map is allowed
// (encoding/json with the map keys it has) but a partial map
// shouldn't reach store.go — ValidateManifest catches it.
func EncodeDaemonHashes(daemonHashes map[string]string) ([]byte, error) {
	if daemonHashes == nil {
		return nil, fmt.Errorf("releaseinstall: nil daemon_hashes")
	}
	names := manifest.SortedHostKeys()
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, name := range names {
		v, ok := daemonHashes[name]
		if !ok {
			return nil, fmt.Errorf("releaseinstall: daemon_hashes missing %s", name)
		}
		if i > 0 {
			buf.WriteByte(',')
		}
		// json.Marshal of the key + value escapes any special
		// chars; daemon names are simple identifiers so escaping
		// is a no-op in practice, but we use json.Marshal for
		// correctness.
		kb, err := json.Marshal(name)
		if err != nil {
			return nil, fmt.Errorf("releaseinstall: marshal key %s: %w", name, err)
		}
		vb, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("releaseinstall: marshal value %s: %w", name, err)
		}
		buf.Write(kb)
		buf.WriteByte(':')
		buf.Write(vb)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// DecodeDaemonHashes parses the JSON output of EncodeDaemonHashes
// (or any other json.Marshal of the same map). Used by store.go
// when reading release_bundles.daemon_hashes back from PostgreSQL.
func DecodeDaemonHashes(body []byte) (map[string]string, error) {
	var out map[string]string
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("releaseinstall: decode daemon_hashes: %w", err)
	}
	return out, nil
}
