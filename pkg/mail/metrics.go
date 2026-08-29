// Metrics seam for pkg/mail. Issue #246 acceptance item 2 wants a
// mail_send_failures_total counter, but pkg/mail is a leaf package and
// must not import pkg/wire (cmd/meterd links pkg/mail without linking
// the daemon metrics package). So the interface lives here and the
// Prometheus implementation registers against a registry the daemon
// injects — the same shape as pkg/snapshothipd/metrics.go.
//
// Every method is nil-safe on both the interface value and the struct
// receiver so tests and dev wiring can leave the seam unset.
package mail

import (
	"errors"

	"github.com/prometheus/client_golang/prometheus"
)

// Failure reasons for RecordFailure. Closed set — pre-instantiated at
// construction so rate() over the series never returns no-data before
// the first failure.
const (
	// ReasonNoTransport is recorded when a daemon resolved a sender that
	// cannot deliver (LogSender/NoopSender on a box that expected a live
	// transport). Issue #246 acceptance item 2.
	ReasonNoTransport = "no_transport"
	// ReasonTransient is a retryable provider failure (5xx, 429, network).
	ReasonTransient = "transient"
	// ReasonPermanent is a non-retryable provider rejection (4xx other
	// than 429 — bad address, unverified domain, revoked key).
	ReasonPermanent = "permanent"
	// ReasonSuppressed is a send skipped because the recipient is on the
	// suppression list (hard bounce or complaint).
	ReasonSuppressed = "suppressed"
)

// Metrics is the observation seam. Implementations must tolerate a nil
// receiver.
type Metrics interface {
	// RecordFailure counts a message that did not reach the provider,
	// labelled by one of the Reason* constants.
	RecordFailure(reason string)
	// RecordRetry counts a single retry attempt against a transport.
	RecordRetry(transport string)
}

// NoopMetrics discards every observation. It is the default when a
// daemon has not wired a registry.
type NoopMetrics struct{}

// RecordFailure implements Metrics.
func (NoopMetrics) RecordFailure(string) {}

// RecordRetry implements Metrics.
func (NoopMetrics) RecordRetry(string) {}

// PrometheusMetrics registers the operator-facing mail counters on a
// daemon's existing registry. The prefix is the daemon name so the
// series follow the ADR-015 "<daemon>_" convention — apid and meterd
// both send mail and each owns its own registry.
type PrometheusMetrics struct {
	sendFailures *prometheus.CounterVec
	retries      *prometheus.CounterVec
}

// NewPrometheusMetrics registers <prefix>_mail_send_failures_total and
// <prefix>_mail_retries_total against reg. Both closed label sets are
// pre-instantiated so the series exist from process start.
func NewPrometheusMetrics(reg prometheus.Registerer, prefix string) (*PrometheusMetrics, error) {
	if reg == nil {
		return nil, errors.New("mail: nil prometheus registerer")
	}
	if prefix == "" {
		return nil, errors.New("mail: empty metrics prefix")
	}
	failures := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: prefix + "_mail_send_failures_total",
		Help: "Outbound emails that did not reach the provider, by reason (issue #246).",
	}, []string{"reason"})
	if err := reg.Register(failures); err != nil {
		return nil, err
	}
	retries := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: prefix + "_mail_retries_total",
		Help: "Retry attempts against the outbound mail transport, by transport.",
	}, []string{"transport"})
	if err := reg.Register(retries); err != nil {
		return nil, err
	}
	for _, reason := range []string{ReasonNoTransport, ReasonTransient, ReasonPermanent, ReasonSuppressed} {
		failures.WithLabelValues(reason)
	}
	for _, transport := range []string{TransportResend, TransportPostmark} {
		retries.WithLabelValues(transport)
	}
	return &PrometheusMetrics{sendFailures: failures, retries: retries}, nil
}

// RecordFailure implements Metrics.
func (m *PrometheusMetrics) RecordFailure(reason string) {
	if m == nil || m.sendFailures == nil {
		return
	}
	m.sendFailures.WithLabelValues(reason).Inc()
}

// RecordRetry implements Metrics.
func (m *PrometheusMetrics) RecordRetry(transport string) {
	if m == nil || m.retries == nil {
		return
	}
	m.retries.WithLabelValues(transport).Inc()
}
