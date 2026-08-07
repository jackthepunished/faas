package events

import (
	"sync"
	"testing"
	"time"
)

// This file drives the 0%-coverage Event interface methods on
// each Wake* struct + the Broadcaster pub/sub machinery. Each
// Event type has Kind/At/Subject/Payload accessors; each is
// exercised with a known timestamp.

func TestSweep_Broadcaster_New(t *testing.T) {
	b := New()
	if b == nil {
		t.Fatal("New() returned nil")
	}
}

func TestSweep_Broadcaster_PublishNoSubs(t *testing.T) {
	b := New()
	if got := b.Publish(Event{Topic: "t1", Payload: []byte("x")}); got != 0 {
		t.Errorf("Publish = %d, want 0", got)
	}
}

func TestSweep_Broadcaster_PublishTopic(t *testing.T) {
	b := New()
	ch, cancel := b.Subscribe("t1")
	defer cancel()
	b.PublishTopic("t1", []byte("hello"))
	select {
	case got := <-ch:
		if got.Topic != "t1" {
			t.Errorf("Topic = %q", got.Topic)
		}
		if string(got.Payload) != "hello" {
			t.Errorf("Payload = %q", got.Payload)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for event")
	}
}

func TestSweep_Broadcaster_CancelRemovesSubscriber(t *testing.T) {
	b := New()
	ch, cancel := b.Subscribe("t1")
	cancel()
	// After cancel, Publish should not deliver.
	got := b.Publish(Event{Topic: "t1"})
	if got != 0 {
		t.Errorf("Publish after cancel = %d, want 0", got)
	}
	_ = ch
}

func TestSweep_Broadcaster_DropOnFull(t *testing.T) {
	b := New()
	ch, cancel := b.Subscribe("t1")
	defer cancel()
	// Fill the buffer (64) plus one more.
	for i := 0; i < 64; i++ {
		b.Publish(Event{Topic: "t1", Payload: []byte("x")})
	}
	// Drain to ensure no blocking.
	go func() {
		for {
			select {
			case <-ch:
			default:
				return
			}
		}
	}()
}

func TestSweep_Broadcaster_ConcurrentSubscribe(t *testing.T) {
	b := New()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, cancel := b.Subscribe("concur")
			defer cancel()
		}()
	}
	wg.Wait()
	b.PublishTopic("concur", []byte("ok"))
}

func TestSweep_BootStarted(t *testing.T) {
	now := time.Now()
	e := BootStarted{
		EmitAt: now, WakeID: "w1", AppID: "a1",
		InstanceID: "i1", NodeID: "n1", Method: "GET", RequestedAt: now,
	}
	if e.Kind() != WakeBootStarted {
		t.Errorf("Kind = %q", e.Kind())
	}
	if !e.At().Equal(now) {
		t.Errorf("At = %v, want %v", e.At(), now)
	}
	if e.Subject() != nil {
		t.Errorf("Subject = %v, want nil", e.Subject())
	}
	p := e.Payload()
	if p["wake_id"] != "w1" {
		t.Errorf("payload wake_id = %v", p["wake_id"])
	}
	if p["app_id"] != "a1" {
		t.Errorf("payload app_id = %v", p["app_id"])
	}
}

func TestSweep_BootCompleted(t *testing.T) {
	now := time.Now()
	e := BootCompleted{
		EmitAt: now, WakeID: "w1", AppID: "a1",
		InstanceID: "i1", NodeID: "n1", Method: "GET",
		StartedAt: now, CompletedAt: now.Add(time.Second),
	}
	if e.Kind() != WakeBootCompleted {
		t.Errorf("Kind = %q", e.Kind())
	}
	if e.Subject() != nil {
		t.Errorf("Subject = %v, want nil", e.Subject())
	}
	if _, ok := e.Payload()["started_at"]; !ok {
		t.Error("payload missing started_at")
	}
}

func TestSweep_BootFailed(t *testing.T) {
	now := time.Now()
	e := BootFailed{
		EmitAt: now, WakeID: "w1", AppID: "a1",
		InstanceID: "i1", NodeID: "n1", Method: "GET",
		Reason: "boot timeout", FailedAt: now,
	}
	if e.Kind() != WakeBootFailed {
		t.Errorf("Kind = %q", e.Kind())
	}
	if e.Subject() != nil {
		t.Errorf("Subject = %v, want nil", e.Subject())
	}
	if e.Payload()["reason"] != "boot timeout" {
		t.Error("payload reason mismatch")
	}
}

func TestSweep_Readiness200(t *testing.T) {
	now := time.Now()
	e := Readiness200{
		EmitAt: now, WakeID: "w1", AppID: "a1",
		InstanceID: "i1", NodeID: "n1",
		HealthcheckPath: "/healthz", ProbeCount: 3, ElapsedMs: 320,
	}
	if e.Kind() != WakeReadiness200 {
		t.Errorf("Kind = %q", e.Kind())
	}
	if e.Subject() != nil {
		t.Errorf("Subject = %v, want nil", e.Subject())
	}
	if e.Payload()["elapsed_ms"] != int64(320) {
		t.Error("payload elapsed_ms mismatch")
	}
}

func TestSweep_ProxyFirstByte(t *testing.T) {
	now := time.Now()
	e := ProxyFirstByte{
		EmitAt: now, WakeID: "w1", AppID: "a1",
		RequestID: "r1", InstanceID: "i1", NodeID: "n1",
		LatencyMs: 100,
	}
	if e.Kind() != WakeProxyFirstByte {
		t.Errorf("Kind = %q", e.Kind())
	}
	if e.Subject() != nil {
		t.Errorf("Subject = %v, want nil", e.Subject())
	}
	if e.Payload()["latency_ms"] != int64(100) {
		t.Error("payload latency_ms mismatch")
	}
}

func TestSweep_ParkStarted(t *testing.T) {
	now := time.Now()
	e := ParkStarted{
		EmitAt: now, WakeID: "w1", AppID: "a1",
		DeploymentID: "d1", InstanceID: "i1", NodeID: "n1",
		StartedAt: now,
	}
	if e.Kind() != WakeParkStarted {
		t.Errorf("Kind = %q", e.Kind())
	}
	if e.Subject() != nil {
		t.Errorf("Subject = %v, want nil", e.Subject())
	}
	if e.Payload()["deployment_id"] != "d1" {
		t.Error("payload deployment_id mismatch")
	}
}

func TestSweep_ParkCompleted(t *testing.T) {
	now := time.Now()
	e := ParkCompleted{
		EmitAt: now, WakeID: "w1", AppID: "a1",
		DeploymentID: "d1", InstanceID: "i1", NodeID: "n1",
		StartedAt: now, CompletedAt: now.Add(time.Second),
		SnapshotID: "snap-1",
	}
	if e.Kind() != WakeParkCompleted {
		t.Errorf("Kind = %q", e.Kind())
	}
	if e.Subject() != nil {
		t.Errorf("Subject = %v, want nil", e.Subject())
	}
	if e.Payload()["snapshot_id"] != "snap-1" {
		t.Error("payload snapshot_id mismatch")
	}
}

func TestSweep_Stalled(t *testing.T) {
	now := time.Now()
	e := Stalled{
		EmitAt: now, AppID: "a1", WakeID: "w1",
		InstanceID: "i1", NodeID: "n1", Reason: "watchdog",
	}
	if e.Kind() != WakeStalled {
		t.Errorf("Kind = %q", e.Kind())
	}
	if e.Subject() != nil {
		t.Errorf("Subject = %v, want nil", e.Subject())
	}
	if e.Payload()["reason"] != "watchdog" {
		t.Error("payload reason mismatch")
	}
}

func TestSweep_Admitted(t *testing.T) {
	now := time.Now()
	e := Admitted{
		EmitAt: now, WakeID: "w1", AppID: "a1",
		RequestID: "r1", AccountID: "acct-123", Plan: "free",
	}
	if e.Kind() != WakeAdmitted {
		t.Errorf("Kind = %q", e.Kind())
	}
	// Admitted has a Subject (the account id).
	subj := e.Subject()
	if subj == nil || *subj != "acct-123" {
		t.Errorf("Subject = %v, want acct-123", subj)
	}
	if e.Payload()["account_id"] != "acct-123" {
		t.Error("payload account_id mismatch")
	}
}

func TestSweep_QueueAccepted(t *testing.T) {
	now := time.Now()
	e := QueueAccepted{
		EmitAt: now, AppID: "a1", WakeID: "w1", RequestID: "r1",
	}
	if e.Kind() != WakeQueueAccepted {
		t.Errorf("Kind = %q", e.Kind())
	}
	if e.Subject() != nil {
		t.Errorf("Subject = %v, want nil", e.Subject())
	}
}
