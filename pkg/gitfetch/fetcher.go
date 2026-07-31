package gitfetch

import (
	"context"
	"io/fs"
)

// Tree is a fetched repository tree. The interface is read-only —
// callers must close the underlying temp dir via Close() to avoid
// leaking disk. repoDecomposition.Phase5 made the lifecycle
// explicit by giving the consumer a Top() method they can hand
// to reposcan.Scan without needing to know the underlying
// filesystem layout.
type Tree interface {
	// FS returns a fs.FS rooted at the extracted archive's
	// top-level. The returned FS is valid for the lifetime of
	// the Tree; closing the Tree invalidates it.
	FS() fs.FS
	// Close removes the temp dir backing the tree. Idempotent —
	// a second Close call is a no-op (Go idiom; `defer
	// tree.Close()` is safe even if the caller already closed).
	Close() error
}

// Fetcher is the package's single production entry point. The
// interface is consumed by cmd/githubd via an adapter that
// resolves the install row + unseals the token (PR-GH.4); the
// fetcher itself stays token-agnostic so the unit tests pin the
// HTTP transport without needing real GitHub credentials.
//
// Implementations:
//   - httpFetcher (pkg/gitfetch/http.go) — the production
//     codeload.github.com client.
//
// All errors are wrapped with a `gitfetch: <op>: %w` prefix and
// the underlying sentinel so callers can match via errors.Is.
type Fetcher interface {
	// Fetch downloads the tar.gz archive for `repoFullName`
	// (owner/name) at `commitSHA` and returns a Tree wrapping
	// the extracted root. The token is scoped to the call —
	// the Fetcher must never store it. The implementation
	// honors ctx.Done() so a cancelled reconcile bails before
	// the archive is fully downloaded.
	Fetch(ctx context.Context, repoFullName, commitSHA, token string) (Tree, error)
}
