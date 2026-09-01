package vmmdgrpc

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	vmmdpb "github.com/onebox-faas/faas/api/proto/onebox/faas/vmmd/v1"
	"golang.org/x/net/http2"
)

func TestStreamBridgeManagerSharesConcurrentStartup(t *testing.T) {
	bridgePath, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(streamBridgePathEnv, bridgePath)

	var starts atomic.Int32
	manager := newTestStreamBridgeManager(t, func() { starts.Add(1) })
	req := &vmmdpb.ForwardHTTPRequestInit{Instance: "instance-a", Port: 8080}

	var wg sync.WaitGroup
	leases := make(chan *streamBridgeLease, 2)
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lease, err := manager.acquire(context.Background(), req, "fc-instance-a")
			if err != nil {
				errs <- err
				return
			}
			leases <- lease
		}()
	}
	wg.Wait()
	close(leases)
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if got := starts.Load(); got != 1 {
		t.Fatalf("bridge starts = %d, want exactly one shared startup", got)
	}
	if got := len(leases); got != 2 {
		t.Fatalf("leases = %d, want two", got)
	}
	for lease := range leases {
		lease.release()
	}
}

func TestStreamBridgeManagerBridgeContextOutlivesRequest(t *testing.T) {
	bridgePath, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(streamBridgePathEnv, bridgePath)

	manager := newTestStreamBridgeManager(t, func() {})
	spawnContextDone := make(chan struct{})
	manager.spawn = func(ctx context.Context, _, _, _, _ string, _ uint32, _ string, _ []string) (*exec.Cmd, *bytes.Buffer, error) {
		go func() {
			<-ctx.Done()
			close(spawnContextDone)
		}()
		cmd := exec.Command("sleep", "60")
		if err := cmd.Start(); err != nil {
			return nil, nil, err
		}
		return cmd, &bytes.Buffer{}, nil
	}

	requestCtx, cancelRequest := context.WithCancel(context.Background())
	req := &vmmdpb.ForwardHTTPRequestInit{Instance: "instance-context", Port: 8080}
	lease, err := manager.acquire(requestCtx, req, "fc-instance-context")
	if err != nil {
		t.Fatal(err)
	}
	lease.release()
	cancelRequest()

	select {
	case <-spawnContextDone:
		t.Fatal("persistent bridge context was canceled with the request")
	case <-time.After(50 * time.Millisecond):
	}

	if err := manager.close(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-spawnContextDone:
	case <-time.After(time.Second):
		t.Fatal("persistent bridge context was not canceled when manager closed")
	}
}

func TestStreamBridgeManagerReapsIdleEntry(t *testing.T) {
	bridgePath, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(streamBridgePathEnv, bridgePath)

	now := time.Now()
	manager := newTestStreamBridgeManager(t, func() {})
	manager.now = func() time.Time { return now }
	manager.idleTimeout = time.Minute
	req := &vmmdpb.ForwardHTTPRequestInit{Instance: "instance-b", Port: 8080}
	lease, err := manager.acquire(context.Background(), req, "fc-instance-b")
	if err != nil {
		t.Fatal(err)
	}
	lease.release()
	if len(manager.entries) != 1 {
		t.Fatalf("entries after release = %d, want one", len(manager.entries))
	}

	now = now.Add(2 * time.Minute)
	manager.reapIdle()
	if len(manager.entries) != 0 {
		t.Fatalf("entries after idle reap = %d, want zero", len(manager.entries))
	}
}

func newTestStreamBridgeManager(t *testing.T, onStart func()) *streamBridgeManager {
	t.Helper()
	manager := newStreamBridgeManager(nil)
	manager.reapInterval = time.Hour
	manager.spawn = func(_ context.Context, _, _, _, _ string, _ uint32, _ string, _ []string) (*exec.Cmd, *bytes.Buffer, error) {
		onStart()
		cmd := exec.Command("sleep", "60")
		if err := cmd.Start(); err != nil {
			return nil, nil, err
		}
		return cmd, &bytes.Buffer{}, nil
	}
	manager.waitSocket = func(string, time.Duration) error { return nil }
	manager.newTransport = func(string) *http2.Transport { return &http2.Transport{} }
	manager.stop = func(_ context.Context, cmd *exec.Cmd, _ *bytes.Buffer) error {
		if cmd.Process == nil {
			return nil
		}
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil
	}
	t.Cleanup(func() { _ = manager.close(context.Background()) })
	return manager
}

func BenchmarkStreamBridgeManagerAcquireReuse(b *testing.B) {
	bridgePath, err := os.Executable()
	if err != nil {
		b.Fatal(err)
	}
	b.Setenv(streamBridgePathEnv, bridgePath)
	manager := newStreamBridgeManager(nil)
	manager.reapInterval = time.Hour
	manager.spawn = func(_ context.Context, _, _, _, _ string, _ uint32, _ string, _ []string) (*exec.Cmd, *bytes.Buffer, error) {
		cmd := exec.Command("sleep", "60")
		if err := cmd.Start(); err != nil {
			return nil, nil, err
		}
		return cmd, &bytes.Buffer{}, nil
	}
	manager.waitSocket = func(string, time.Duration) error { return nil }
	manager.newTransport = func(string) *http2.Transport { return &http2.Transport{} }
	manager.stop = func(_ context.Context, cmd *exec.Cmd, _ *bytes.Buffer) error {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil
	}
	b.Cleanup(func() { _ = manager.close(context.Background()) })

	req := &vmmdpb.ForwardHTTPRequestInit{Instance: "benchmark-instance", Port: 8080}
	lease, err := manager.acquire(context.Background(), req, "fc-benchmark-instance")
	if err != nil {
		b.Fatal(err)
	}
	lease.release()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		lease, err := manager.acquire(context.Background(), req, "fc-benchmark-instance")
		if err != nil {
			b.Fatal(err)
		}
		lease.release()
	}
	b.StopTimer()
}
