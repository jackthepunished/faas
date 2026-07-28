package sched

import "io"

// fakeLogStream is the shared no-op LogStream used by every
// sched-side test fake (egress_drift, heartbeat, vmmrouter,
// instancestats). It returns io.EOF on the first Recv so callers
// that loop on Recv exit cleanly.
//
// Tests that exercise the live-tail path (issue #254 / Move 4)
// inject a custom LogStream via a dedicated LogsFn hook on the
// per-test fake. The Move 4 schedd-side handler test in
// pkg/scheddgrpc uses the bufconn-backed pkg/vmmdgrpc handler so
// it doesn't go through this fake at all.
type fakeLogStream struct{}

// Recv returns io.EOF so the caller's stream loop exits.
func (s *fakeLogStream) Recv() (LogLine, error) { return LogLine{}, io.EOF }
