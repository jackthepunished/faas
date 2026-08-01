// Source-tree interface for githubd's push-dispatch path (PR-H,
// repo decomposition Phase 5).
//
// githubd.Service.HandlePushRequest needs to read the on-disk tree
// for the pushed commit so pkg/reconcile.Scan can run on it. The
// transport (HTTP codeload archive + tar.gz extraction) lives in
// pkg/gitfetch (PR-GH.3) — credential-free, fs.FS-shaped. The
// adapter that closes the loop (resolve install row → unseal
// token → call gitfetch.Fetcher → hand back a Tree) lives in
// cmd/githubd/source_fetcher.go and implements SourceFetcher.
//
// Splitting the interface from the implementation keeps pkg/githubd
// free of pgx/age/secretbox imports (the package has unit tests
// that exercise HandlePushRequest end-to-end without a real DB or
// real age keypair). The interface is also the seam future slices
// need: PR-H.2 replaces codeload with a self-hosted mirror by
// swapping the concrete implementation behind this interface.

package githubd

import (
	"context"
	"io/fs"
)

// SourceTree is the read-only filesystem view of one commit's tree
// after the fetcher has downloaded and extracted the archive. The
// interface intentionally mirrors gitfetch.Tree so the cmd/githubd
// adapter can hand the package-local value straight through. We
// re-declare it here so pkg/githubd stays decoupled from
// pkg/gitfetch at the type-system level — a future migration off
// pkg/gitfetch doesn't require editing pkg/githubd.
type SourceTree interface {
	FS() fs.FS
	Close() error
}

// SourceFetcher downloads + extracts the source tree for one
// (accountID, installID, repoFullName, commitSHA) tuple.
// Implementations must:
//
//   - Use accountID to look up the durable install row (the
//     account → install mapping is one-to-one per §11 oauth-handshake
//     model). The install row carries the sealed install token that
//     authenticates the archive download.
//   - Verify installID matches the install row's InstallationID.
//     Mismatch is a hard error (ErrNoBinding) — a malicious push
//     that lies about its install_id must never reach the archive
//     endpoint under the wrong install's token (PR-A's audit
//     pipeline rejects takeover attempts, but the daemon-side guard
//     is load-bearing).
//   - Scope the bearer token to a single Fetch call. The token must
//     NEVER be stored on the receiver; the cmd/githubd adapter
//     unseals it on demand and the production Fetcher uses it for
//     exactly one Authorization header per Fetch.
//   - Honour ctx cancellation. A reconcile that bails out must not
//     leave the fetcher spinning until its own timeout.
//   - Return a Tree whose Close() is idempotent. githubd's webhook
//     handler defers tree.Close() so a panic in reconcile doesn't
//     leak the temp dir.
type SourceFetcher interface {
	Fetch(ctx context.Context, accountID string, installID int64, repoFullName, commitSHA string) (SourceTree, error)
}
