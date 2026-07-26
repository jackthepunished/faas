module github.com/poyrazK/faas-go

go 1.23

// Issue #266, PR 2. The SDK module is a leaf: it contains only the
// pkg/api/* files copied into ./internal/api, and those files use only
// stdlib (no github.com/onebox-faas/faas/* imports). The public surface
// at ./client.go (PR 3) re-exports the internal types under package
// `faas`.
//
// No `require` for the root module here. The split in PR 12 will
// reverse: the daemon's root go.mod adds
// `require github.com/poyrazK/faas-go vX.Y.Z`, and pkg/api/* is trimmed
// to only the daemon-only files.
