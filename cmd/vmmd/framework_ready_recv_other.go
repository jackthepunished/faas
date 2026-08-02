//go:build !linux

// Stub for non-linux builds (macOS dev boxes, CI cargo / cross-
// compile). The DGRAM recv loop is linux-only because AF_VSOCK
// is a Linux kernel feature. The cmd main() calls the
// linux-stubbed StartFrameworkReadyReceiver; on non-linux we
// return an error and the cmd main logs and continues with a
// no-op receiver (the engine's warm-capture wait in PR #470-FU-A
// falls through to init-tier on the no-signal path).

package main

import (
	"fmt"
	"log/slog"
	"sync/atomic"

	"github.com/onebox-faas/faas/pkg/fcvm"
)

// FrameworkReadyReceiver is the host-side DGRAM listener. The
// non-linux stub is a nil-receiver so cmd/vmmd/main.go's
// defer recv.Close() compiles on every platform. fd is an
// atomic.Int32 to mirror the linux type exactly (CRIT-related
// review feedback on PR #470-FU-B).
type FrameworkReadyReceiver struct {
	fd  atomic.Int32
	log *slog.Logger
	mgr *fcvm.Manager
}

// Close is a no-op on non-linux platforms.
func (r *FrameworkReadyReceiver) Close() {}

// StartFrameworkReadyReceiver returns an error on non-linux
// platforms. The cmd main() soft-fails on this path so the dev
// box can still bring up vmmd without AF_VSOCK support.
func StartFrameworkReadyReceiver(_ *slog.Logger, _ *fcvm.Manager) (*FrameworkReadyReceiver, error) {
	return nil, fmt.Errorf("framework_ready DGRAM recv: AF_VSOCK requires Linux")
}
