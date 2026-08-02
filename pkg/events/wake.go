// wake.go — canonical wake-timeline vocabulary (issue #517 / PR-C, AC2).
//
// One row per wake phase. Each struct implements WakeEvent so the
// Platform.Emit driver (platform.go) writes a single, typed row that
// the customer-facing GET /v1/apps/{slug}/wakes/{wake_id}/timeline
// endpoint can read back without a hand-rolled jsonb filter. The
// payload field names are the same wire names the endpoint surfaces
// in the `data` object — rename here, rename in the docs, rename in
// the SDK.
//
// Naming follows the spec §5.1 audit-event taxonomy prefixed with
// `wake.` so a single §12 panel selector (`kind_prefix=wake.`)
// captures the whole wake lifecycle. The 13 kinds cover the full
// wake envelope (queue → admit → boot → readiness → proxy → park /
// build / deploy), and the legacy bare names (`state_transition`,
// `wake_boot_error`, `park_snapshot_error`, `watchdog_timeout`,
// `app.characterized`, `cron.fired`, `reaper_scale_down`,
// `stateless.advisory`, `app.signed_image_accepted`,
// `app.signature_missing`, `app.signature_invalid`) stay
// unchanged — PR-C is additive, see ADR-064 §"Compatibility".
//
// Why struct types and not `kind string + data map[string]any`:
// the Go compiler is the cheapest schema validator. A new field
// added to a payload struct lights up call sites that need to
// update; a new key in a map literal ships silently.
package events

import "time"

// Wake-timeline vocabulary (issue #517 / PR-C, ADR-064). Constants
// are the canonical kind strings written to events.kind. The
// commented payload schemas below mirror the typed struct fields
// exactly — keep them in sync when adding a field.
const (
	// WakeQueueAccepted — schedd accepted the wake into the queue
	// (regular Wake RPC or cron dispatch boundary). Payload:
	// {wake_id, app_id, request_id, queue_wait_ms}.
	WakeQueueAccepted = "wake.queue_accepted"
	// WakeAdmitted — schedd admitted the wake past the per-app
	// concurrency gate. Payload: {wake_id, app_id, request_id,
	// account_id, plan, admitted_at}.
	WakeAdmitted = "wake.admitted"
	// WakeBootStarted — schedd started a boot (snapshot restore
	// or cold boot). Mirrored by vmmd on CreateFromSnapshot /
	// Wake. Payload: {wake_id, app_id, instance_id, node_id,
	// method, requested_at}.
	WakeBootStarted = "wake.boot_started"
	// WakeBootCompleted — schedd post-RecordRuntime; the instance
	// is now RUNNING. Sibling of the existing `app.characterized`
	// audit row (different timings — `app.characterized` follows
	// after the first request lands, this fires on RUNNING).
	// Payload: {wake_id, app_id, instance_id, node_id, method,
	// started_at, completed_at}.
	WakeBootCompleted = "wake.boot_completed"
	// WakeBootFailed — boot path failed. Sibling of the legacy
	// `wake_boot_error` audit row. Payload: {wake_id, app_id,
	// instance_id, node_id, method, reason, failed_at}.
	WakeBootFailed = "wake.boot_failed"
	// WakeReadiness200 — vmmd's waitReady saw its first 2xx probe
	// (pkg/fcvm/vmm.go). The most load-bearing new code path: today
	// there is no success-side log on readiness_200, and the §12
	// wake-latency panel derives its p50/p95/p99 from this row.
	// Dedupe guard: the first 2xx only; defer at the
	// return-point. Payload: {wake_id, app_id, instance_id,
	// node_id, healthcheck_path, probe_count, elapsed_ms}.
	WakeReadiness200 = "wake.readiness_200"
	// WakeProxyFirstByte — gatewayd received the first response
	// byte from the woken instance (httptrace.GotFirstResponseByte
	// callback). Payload: {wake_id, app_id, request_id,
	// instance_id, node_id, latency_ms}.
	WakeProxyFirstByte = "wake.proxy_first_byte"
	// WakeParkStarted — schedd transitioning the instance to
	// SNAPSHOTTING. Payload: {wake_id, app_id, instance_id,
	// node_id, started_at}.
	WakeParkStarted = "wake.park_started"
	// WakeParkCompleted — snapshot succeeded; instance is PARKED.
	// Dual of WakeParkStarted. Payload: {wake_id, app_id,
	// instance_id, node_id, started_at, completed_at, snapshot_id}.
	WakeParkCompleted = "wake.park_completed"
	// WakeStalled — watchdog path: instance hasn't transitioned
	// states within the deadline. Sibling of the legacy
	// `watchdog_timeout` audit row — both fire, joined by
	// wake_id. Payload: {wake_id, app_id, instance_id, node_id,
	// reason, at}.
	WakeStalled = "wake.stalled"
	// WakeBuildSucceeded — builderd finished a build (ADR-030).
	// Payload: {app_id, deployment_id, image_digest, duration_ms}.
	WakeBuildSucceeded = "wake.build_succeeded"
	// WakeBuildFailed — builderd build failed. Payload: {app_id,
	// deployment_id, image_digest, reason}.
	WakeBuildFailed = "wake.build_failed"
	// WakeDeployFailed — apid's deploy rollback path. Payload:
	// {app_id, deployment_id, reason}.
	WakeDeployFailed = "wake.deploy_failed"
)

// WakeEvent is the contract pkg/events.Platform.Emit consumes. The
// concrete payload structs below (QueueAccepted, Admitted,
// BootStarted, etc.) are the only emitters — callers MUST instantiate
// one rather than rolling their own struct. The one-method interface
// makes the row schema the compiler's problem, not the daemon's.
type WakeEvent interface {
	// Kind is the events.kind string value (e.g. "wake.boot_started").
	Kind() string
	// At is the wall-clock timestamp the row carries. schedd's
	// transitionWithKind precedent sets this off the engine clock
	// so the timeline reads forward even when the goroutine that
	// emits the row is delayed.
	At() time.Time
	// Subject is the optional accounts.id (UUID) the row is
	// attributed to. nil for system-level events (e.g. cron.fired
	// when account resolution failed earlier in the path). Matches
	// the pkg/audit.Auditor.Emit contract.
	Subject() *string
	// Payload is the typed struct marshaled to jsonb on the
	// events.data column. The Platform driver calls
	// json.Marshal; the struct fields ARE the JSON keys.
	Payload() map[string]any
}

// addrString is a tiny helper so payload structs can express a
// subject pointer without forcing every constructor to write
// `if x != "" { return &x }`. Used by the typed structs below.
func addrString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// QueueAccepted — schedd accepted the wake into the queue. Fires
// from both the regular Wake RPC (pkg/sched/engine.go) and the
// cron dispatch boundary (pkg/sched/loop.go) so the customer-facing
// timeline surfaces both paths joined by request_id.
type QueueAccepted struct {
	EmitAt      time.Time
	WakeID      string
	AppID       string
	RequestID   string
	QueueWaitMs int64
}

func (e QueueAccepted) Kind() string     { return WakeQueueAccepted }
func (e QueueAccepted) At() time.Time    { return e.EmitAt }
func (e QueueAccepted) Subject() *string { return nil }
func (e QueueAccepted) Payload() map[string]any {
	return map[string]any{
		"wake_id":       e.WakeID,
		"app_id":        e.AppID,
		"request_id":    e.RequestID,
		"queue_wait_ms": e.QueueWaitMs,
	}
}

// Admitted — schedd passed the per-app concurrency gate. The
// account_id + plan fields surface in the payload so the operator
// view can group wake latency by plan (the §12 dashboard's wake
// latency panel groups by plan to surface a Hobby-tier degradation).
type Admitted struct {
	EmitAt    time.Time
	WakeID    string
	AppID     string
	RequestID string
	AccountID string
	Plan      string
}

func (e Admitted) Kind() string     { return WakeAdmitted }
func (e Admitted) At() time.Time    { return e.EmitAt }
func (e Admitted) Subject() *string { return addrString(e.AccountID) }
func (e Admitted) Payload() map[string]any {
	return map[string]any{
		"wake_id":    e.WakeID,
		"app_id":     e.AppID,
		"request_id": e.RequestID,
		"account_id": e.AccountID,
		"plan":       e.Plan,
	}
}

// BootStarted — schedd started a boot. method is the scheddpb
// wake method (WAKE_RESTORE / WAKE_COLD_BOOT) as a string so the
// payload is self-describing without dragging the protobuf package
// into the wire shape.
type BootStarted struct {
	EmitAt      time.Time
	WakeID      string
	AppID       string
	InstanceID  string
	NodeID      string
	Method      string
	RequestedAt time.Time
}

func (e BootStarted) Kind() string     { return WakeBootStarted }
func (e BootStarted) At() time.Time    { return e.EmitAt }
func (e BootStarted) Subject() *string { return nil }
func (e BootStarted) Payload() map[string]any {
	return map[string]any{
		"wake_id":      e.WakeID,
		"app_id":       e.AppID,
		"instance_id":  e.InstanceID,
		"node_id":      e.NodeID,
		"method":       e.Method,
		"requested_at": e.RequestedAt.UTC(),
	}
}

// BootCompleted — schedd post-RecordRuntime. Distinct from the
// legacy `app.characterized` audit row, which fires when the first
// request lands; this row fires when the instance enters RUNNING,
// so the wake timeline is correct even on apps that never receive
// a request.
type BootCompleted struct {
	EmitAt      time.Time
	WakeID      string
	AppID       string
	InstanceID  string
	NodeID      string
	Method      string
	StartedAt   time.Time
	CompletedAt time.Time
}

func (e BootCompleted) Kind() string     { return WakeBootCompleted }
func (e BootCompleted) At() time.Time    { return e.EmitAt }
func (e BootCompleted) Subject() *string { return nil }
func (e BootCompleted) Payload() map[string]any {
	return map[string]any{
		"wake_id":      e.WakeID,
		"app_id":       e.AppID,
		"instance_id":  e.InstanceID,
		"node_id":      e.NodeID,
		"method":       e.Method,
		"started_at":   e.StartedAt.UTC(),
		"completed_at": e.CompletedAt.UTC(),
	}
}

// BootFailed — boot path failed. The reason string is the same
// value schedd's transitionWithKind passes to the legacy
// `wake_boot_error` audit row so the legacy + typed rows are
// joinable on (wake_id, reason) for the operator's debug view.
type BootFailed struct {
	EmitAt     time.Time
	WakeID     string
	AppID      string
	InstanceID string
	NodeID     string
	Method     string
	Reason     string
	FailedAt   time.Time
}

func (e BootFailed) Kind() string     { return WakeBootFailed }
func (e BootFailed) At() time.Time    { return e.EmitAt }
func (e BootFailed) Subject() *string { return nil }
func (e BootFailed) Payload() map[string]any {
	return map[string]any{
		"wake_id":     e.WakeID,
		"app_id":      e.AppID,
		"instance_id": e.InstanceID,
		"node_id":     e.NodeID,
		"method":      e.Method,
		"reason":      e.Reason,
		"failed_at":   e.FailedAt.UTC(),
	}
}

// Readiness200 — vmmd's waitReady saw its first 2xx probe. The
// ProbeCount / ElapsedMs fields let the operator view a wake
// timeline and see "the readiness probe took 4 attempts and 320ms"
// without joining against the vmmd log.
type Readiness200 struct {
	EmitAt          time.Time
	WakeID          string
	AppID           string
	InstanceID      string
	NodeID          string
	HealthcheckPath string
	ProbeCount      int
	ElapsedMs       int64
}

func (e Readiness200) Kind() string     { return WakeReadiness200 }
func (e Readiness200) At() time.Time    { return e.EmitAt }
func (e Readiness200) Subject() *string { return nil }
func (e Readiness200) Payload() map[string]any {
	return map[string]any{
		"wake_id":          e.WakeID,
		"app_id":           e.AppID,
		"instance_id":      e.InstanceID,
		"node_id":          e.NodeID,
		"healthcheck_path": e.HealthcheckPath,
		"probe_count":      e.ProbeCount,
		"elapsed_ms":       e.ElapsedMs,
	}
}

// ProxyFirstByte — gatewayd received the first response byte from
// the woken instance. LatencyMs is the wall-clock from queue
// acceptance to first byte so the customer timeline shows the
// end-to-end latency, not just the proxy hop.
type ProxyFirstByte struct {
	EmitAt     time.Time
	WakeID     string
	AppID      string
	RequestID  string
	InstanceID string
	NodeID     string
	LatencyMs  int64
}

func (e ProxyFirstByte) Kind() string     { return WakeProxyFirstByte }
func (e ProxyFirstByte) At() time.Time    { return e.EmitAt }
func (e ProxyFirstByte) Subject() *string { return nil }
func (e ProxyFirstByte) Payload() map[string]any {
	return map[string]any{
		"wake_id":     e.WakeID,
		"app_id":      e.AppID,
		"request_id":  e.RequestID,
		"instance_id": e.InstanceID,
		"node_id":     e.NodeID,
		"latency_ms":  e.LatencyMs,
	}
}

// ParkStarted — schedd transitioning the instance to
// SNAPSHOTTING. The dual WakeParkCompleted row carries the snapshot_id.
type ParkStarted struct {
	EmitAt     time.Time
	WakeID     string
	AppID      string
	InstanceID string
	NodeID     string
	StartedAt  time.Time
}

func (e ParkStarted) Kind() string     { return WakeParkStarted }
func (e ParkStarted) At() time.Time    { return e.EmitAt }
func (e ParkStarted) Subject() *string { return nil }
func (e ParkStarted) Payload() map[string]any {
	return map[string]any{
		"wake_id":     e.WakeID,
		"app_id":      e.AppID,
		"instance_id": e.InstanceID,
		"node_id":     e.NodeID,
		"started_at":  e.StartedAt.UTC(),
	}
}

// ParkCompleted — snapshot succeeded; instance is PARKED.
type ParkCompleted struct {
	EmitAt      time.Time
	WakeID      string
	AppID       string
	InstanceID  string
	NodeID      string
	StartedAt   time.Time
	CompletedAt time.Time
	SnapshotID  string
}

func (e ParkCompleted) Kind() string     { return WakeParkCompleted }
func (e ParkCompleted) At() time.Time    { return e.EmitAt }
func (e ParkCompleted) Subject() *string { return nil }
func (e ParkCompleted) Payload() map[string]any {
	return map[string]any{
		"wake_id":      e.WakeID,
		"app_id":       e.AppID,
		"instance_id":  e.InstanceID,
		"node_id":      e.NodeID,
		"started_at":   e.StartedAt.UTC(),
		"completed_at": e.CompletedAt.UTC(),
		"snapshot_id":  e.SnapshotID,
	}
}

// Stalled — watchdog path. The existing `watchdog_timeout` audit
// row stays unchanged for GDPR-export compatibility; this row
// carries the structured payload so the customer-facing timeline
// surfaces the same event with a typed shape.
type Stalled struct {
	EmitAt     time.Time
	WakeID     string
	AppID      string
	InstanceID string
	NodeID     string
	Reason     string
}

func (e Stalled) Kind() string     { return WakeStalled }
func (e Stalled) At() time.Time    { return e.EmitAt }
func (e Stalled) Subject() *string { return nil }
func (e Stalled) Payload() map[string]any {
	return map[string]any{
		"wake_id":     e.WakeID,
		"app_id":      e.AppID,
		"instance_id": e.InstanceID,
		"node_id":     e.NodeID,
		"reason":      e.Reason,
	}
}

// BuildSucceeded — builderd finished a build. ImageDigest is the
// OCI digest of the resulting image so the timeline can join
// against the deployment row.
type BuildSucceeded struct {
	EmitAt       time.Time
	AppID        string
	DeploymentID string
	ImageDigest  string
	DurationMs   int64
}

func (e BuildSucceeded) Kind() string     { return WakeBuildSucceeded }
func (e BuildSucceeded) At() time.Time    { return e.EmitAt }
func (e BuildSucceeded) Subject() *string { return nil }
func (e BuildSucceeded) Payload() map[string]any {
	return map[string]any{
		"app_id":        e.AppID,
		"deployment_id": e.DeploymentID,
		"image_digest":  e.ImageDigest,
		"duration_ms":   e.DurationMs,
	}
}

// BuildFailed — builderd build failed. ImageDigest is the digest
// of the partially-built image (empty string if the build failed
// before the image was committed).
type BuildFailed struct {
	EmitAt       time.Time
	AppID        string
	DeploymentID string
	ImageDigest  string
	Reason       string
}

func (e BuildFailed) Kind() string     { return WakeBuildFailed }
func (e BuildFailed) At() time.Time    { return e.EmitAt }
func (e BuildFailed) Subject() *string { return nil }
func (e BuildFailed) Payload() map[string]any {
	return map[string]any{
		"app_id":        e.AppID,
		"deployment_id": e.DeploymentID,
		"image_digest":  e.ImageDigest,
		"reason":        e.Reason,
	}
}

// DeployFailed — apid's deploy rollback path. The reason string
// is the operator-facing error (e.g. "image_scan_failed",
// "cosign_signature_invalid") so the timeline can group by reason.
type DeployFailed struct {
	EmitAt       time.Time
	AppID        string
	DeploymentID string
	Reason       string
}

func (e DeployFailed) Kind() string     { return WakeDeployFailed }
func (e DeployFailed) At() time.Time    { return e.EmitAt }
func (e DeployFailed) Subject() *string { return nil }
func (e DeployFailed) Payload() map[string]any {
	return map[string]any{
		"app_id":        e.AppID,
		"deployment_id": e.DeploymentID,
		"reason":        e.Reason,
	}
}
