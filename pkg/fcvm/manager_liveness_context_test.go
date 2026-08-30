package fcvm

import (
	"context"
	"testing"
	"time"
)

func TestProcessExitedRelaysWithoutDroppingManagerEntry(t *testing.T) {
	manager := NewManager(&fakeRunner{}, &fakeVMM{}, Paths{Kernel: "/k"}, "test", nil, nil)
	var gotInstance, gotReason string
	manager.WithLivenessSink(func(_ context.Context, instanceID, reason string) {
		gotInstance, gotReason = instanceID, reason
	})
	manager.RegisterInstanceForTest("vm-exited", "deployment-exited")

	manager.ProcessExited("vm-exited", 137)

	if gotInstance != "vm-exited" || gotReason != "process_exited" {
		t.Fatalf("relay = (%q, %q), want (vm-exited, process_exited)",
			gotInstance, gotReason)
	}
	if manager.LiveCount() != 1 {
		t.Fatalf("LiveCount = %d, want 1 until schedd destroys the row",
			manager.LiveCount())
	}
}

func TestProcessExitedDuringWakeHandoffIsRemembered(t *testing.T) {
	manager := NewManager(&fakeRunner{}, &fakeVMM{}, Paths{Kernel: "/k"}, "test", nil, nil)
	manager.mu.Lock()
	manager.waking["vm-handoff"] = struct{}{}
	manager.mu.Unlock()

	manager.ProcessExited("vm-handoff", 137)

	manager.mu.Lock()
	exitCode, ok := manager.pendingProcessExits["vm-handoff"]
	delete(manager.pendingProcessExits, "vm-handoff")
	delete(manager.waking, "vm-handoff")
	manager.mu.Unlock()
	if !ok || exitCode != 137 {
		t.Fatalf("pending exit = (%d, %v), want (137, true)", exitCode, ok)
	}
}

func TestProcessExitedAfterTeardownDoesNotCreatePendingExit(t *testing.T) {
	manager := NewManager(&fakeRunner{}, &fakeVMM{}, Paths{Kernel: "/k"}, "test", nil, nil)
	manager.ProcessExited("vm-teardown", 137)

	manager.mu.Lock()
	_, ok := manager.pendingProcessExits["vm-teardown"]
	manager.mu.Unlock()
	if ok {
		t.Fatal("teardown exit created a pending wake marker")
	}
}

func TestProcessExitedIgnoresBuilder(t *testing.T) {
	manager := NewManager(&fakeRunner{}, &fakeVMM{}, Paths{Kernel: "/k"}, "test", nil, nil)
	var relayed bool
	manager.WithLivenessSink(func(context.Context, string, string) { relayed = true })
	manager.mu.Lock()
	manager.live["builder"] = &Instance{Lease: Lease{Instance: "builder", IsBuilder: true}}
	manager.mu.Unlock()

	manager.ProcessExited("builder", 0)
	if relayed {
		t.Fatal("builder exit was relayed as an app liveness failure")
	}
}

func TestLivenessLoopUsesManagerLifecycleContext(t *testing.T) {
	requestCtx, cancelRequest := context.WithCancel(context.Background())
	defer cancelRequest()
	lifecycleCtx, cancelLifecycle := context.WithCancel(context.Background())
	defer cancelLifecycle()

	manager := NewManager(&fakeRunner{}, &fakeVMM{}, Paths{Kernel: "/k"}, "test", nil, nil).
		WithLifecycleContext(lifecycleCtx)
	registry := NewLivenessRegistry()
	var parent context.Context
	manager.WithLivenessProbes(registry, LivenessProbeConfig{
		PeriodSeconds:       1,
		ConsecutiveFailures: 2,
	}).WithLivenessProbeStarter(func(ctx context.Context, _ string, _ int, _ string, _ LivenessProbeConfig) context.CancelFunc {
		parent = ctx
		return func() {}
	})
	manager.RegisterInstanceForTest("vm-ctx", "deployment-ctx")

	manager.startLivenessLoop(requestCtx, "vm-ctx", 1, nil)
	if parent == nil {
		t.Fatal("liveness starter was not called")
	}
	cancelRequest()
	if err := parent.Err(); err != nil {
		t.Fatalf("request cancellation canceled liveness parent: %v", err)
	}
	cancelLifecycle()
	deadline := time.Now().Add(time.Second)
	for parent.Err() == nil && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if parent.Err() != context.Canceled {
		t.Fatalf("lifecycle cancellation = %v, want context canceled", parent.Err())
	}
	manager.cancelLivenessLoop("vm-ctx")
}
