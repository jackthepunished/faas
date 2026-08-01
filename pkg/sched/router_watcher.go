// router_watcher.go — Tier A3: live-refresh VMMRouter.targets on
// every compute_node_changed pg_notify payload.
//
// Why this exists: schedd's VMMRouter (see vmmrouter.go) fills its
// targets map once at NewEngine via store.ActiveComputeNodes and
// never re-asks the store. After any compute_nodes UPSERT — admin
// URL rotation, active flip, soft-delete — schedd keeps dialling
// the stale target_url until a daemon restart. This watcher closes
// that gap by subscribing to the same channel gatewayd's
// NodeClientCache uses (cmd/gatewayd/nodecache.go::WatchEvictions)
// and drops the dialed client + reloads target_url on every
// payload.
//
// Why "every JSON hit" (not active-flip only): the payload carries
// only {"node_id","active"} — no target_url field. A pure
// target_url UPDATE leaves active unchanged, so an active-flip
// filter would miss it. Refresh-on-every-hit is idempotent and the
// only correct policy: a no-op UPSERT costs one SELECT, and a
// consumer that filters "what counts as a change" from a
// producer's choice drifts the moment the producer adds a column.
//
// NOTE: the compute_node_changed channel carries a MIXED payload
// shape today:
//
//	- migrations/00026_compute_node_notify.sql emits JSON
//	  {"node_id":"<uuid>","active":<bool>} via the
//	  compute_node_changed_trg AFTER INSERT OR UPDATE trigger
//	  on compute_nodes.
//	- migrations/00076_compute_node_keys.sql emits the literal
//	  string "compute_node_keys" via a second trigger on
//	  compute_node_keys (TG_TABLE_NAME).
//
// pkg/db/notify.go:191-197 documents only the JSON shape — that
// doc drift is filed separately and not addressed by this PR.
// The watcher tolerates both shapes: a JSON-string payload that is
// not a node event is dropped with a Warn.

package sched

import (
	"context"
	"encoding/json"

	"github.com/onebox-faas/faas/pkg/db"
)

// RouterWatcherLogger is the minimal slog surface this watcher
// needs. Mirrors pkg/sched/nodekeys.go::NodeKeyLogger — tests pass
// nil and the watcher logs nothing.
type RouterWatcherLogger interface {
	Warn(msg string, args ...any)
	Info(msg string, args ...any)
}

// RouterRefreshFunc is the per-payload hook. It is responsible for
// looking up the live row (or its absence), updating the router,
// and returning any error. The watcher logs but does not exit on
// a non-nil error — see RunRouterRefreshWatcher for the
// failure-mode contract.
//
// ctx is the watcher-supplied context; refresh should be honoured
// promptly so a wedged PG does not block the for-select loop.
// The production wiring (cmd/schedd/main.go) calls
// store.ComputeNodeByID inside this closure and then
// vmmRouter.Refresh(nodeID, targetURL) on the result.
type RouterRefreshFunc func(ctx context.Context, nodeID string) error

// nodeChangedPayload mirrors the JSON migration 00026 emits.
// Fields are decoded in order; an unknown extra field is dropped
// silently (the channel payload is internal, not a customer wire
// shape, so `json.Decoder.DisallowUnknownFields` is overkill).
type nodeChangedPayload struct {
	NodeID string `json:"node_id"`
	Active bool   `json:"active"`
}

// isRawJSONObject peeks at the payload and returns true only when
// it parses as a JSON object literal. A bare JSON string
// ("compute_node_keys") parses as json.RawMessage but fails the
// second-unmarshal-into-struct probe, so we use the probe to
// distinguish the two shapes without speculatively parsing.
//
//	n.RawMessage:  '{...}' or '"..."'
//	probe A:        json.Unmarshal(RawMessage, &RawMessage) — always passes
//	probe B:        json.Unmarshal(RawMessage, &struct{...}) — fails for the literal
func isNodeEventPayload(raw []byte) (nodeChangedPayload, bool) {
	var p nodeChangedPayload
	if err := json.Unmarshal(raw, &p); err != nil || p.NodeID == "" {
		return nodeChangedPayload{}, false
	}
	return p, true
}

// RunRouterRefreshWatcher drains `notif` until ctx is cancelled
// or the channel is closed. On every well-formed
// compute_node_changed payload (JSON shape) it calls refresh with
// the row's last-known target_url. Payloads that are not node
// events (the literal "compute_node_keys" from migration 00076,
// or malformed JSON) are dropped with a Warn — never propagated
// to the caller.
//
// Failure modes:
//
//   - refresh returns error: log Warn, continue. A transient PG
//     blip must not stop the loop on the next notify.
//   - channel closed (SubscribeWithReconnect inner reconnect
//     collapses the wrapper's outer channel — should not happen
//     while ctx is alive): return cleanly.
//   - ctx cancelled: return.
//
// A nil log is allowed (silent).
func RunRouterRefreshWatcher(ctx context.Context, log RouterWatcherLogger, notif <-chan db.Notification, refresh RouterRefreshFunc) {
	if refresh == nil {
		// No-op watcher: drain + drop until ctx cancel. The
		// caller probably mis-wired; not a panic-condition.
		<-ctx.Done()
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case n, ok := <-notif:
			if !ok {
				return
			}
			raw := n.Payload
			p, isNode := isNodeEventPayload([]byte(raw))
			if !isNode {
				if log != nil {
					log.Warn("sched: router refresh watcher: dropping non-node payload",
						"channel", n.Channel, "payload", raw)
				}
				continue
			}
			// wire the refresh through the supplied closure;
			// the wiring in cmd/schedd reads
			// store.ComputeNodeByID and forwards either the
			// live target_url or an empty string on row-gone.
			if err := refresh(ctx, p.NodeID); err != nil {
				if log != nil {
					log.Warn("sched: router refresh: live row read failed",
						"node_id", p.NodeID, "active", p.Active, "err", err)
				}
				continue
			}
		}
	}
}
