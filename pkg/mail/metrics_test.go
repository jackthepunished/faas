package mail

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestNewPrometheusMetrics_Validation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		reg     prometheus.Registerer
		prefix  string
		wantErr bool
	}{
		{name: "ok", reg: prometheus.NewRegistry(), prefix: "apid"},
		{name: "nil registry", reg: nil, prefix: "apid", wantErr: true},
		{name: "empty prefix", reg: prometheus.NewRegistry(), prefix: "", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m, err := NewPrometheusMetrics(tc.reg, tc.prefix)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got nil (m=%v)", m)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if m == nil {
				t.Fatal("want non-nil metrics")
			}
		})
	}
}

// The closed label sets must exist from process start so a PromQL
// rate() over them never returns no-data before the first failure.
func TestNewPrometheusMetrics_PreInstantiatesClosedLabelSets(t *testing.T) {
	t.Parallel()
	reg := prometheus.NewRegistry()
	if _, err := NewPrometheusMetrics(reg, "apid"); err != nil {
		t.Fatalf("NewPrometheusMetrics: %v", err)
	}
	got := testutil.CollectAndCount(reg, "apid_mail_send_failures_total")
	if want := 4; got != want {
		t.Errorf("send_failures series = %d, want %d", got, want)
	}
	if got, want := testutil.CollectAndCount(reg, "apid_mail_retries_total"), 2; got != want {
		t.Errorf("retries series = %d, want %d", got, want)
	}
}

func TestPrometheusMetrics_RecordFailure(t *testing.T) {
	t.Parallel()
	reg := prometheus.NewRegistry()
	m, err := NewPrometheusMetrics(reg, "apid")
	if err != nil {
		t.Fatalf("NewPrometheusMetrics: %v", err)
	}
	m.RecordFailure(ReasonNoTransport)
	m.RecordFailure(ReasonNoTransport)
	m.RecordFailure(ReasonTransient)

	const expected = `
# HELP apid_mail_send_failures_total Outbound emails that did not reach the provider, by reason (issue #246).
# TYPE apid_mail_send_failures_total counter
apid_mail_send_failures_total{reason="no_transport"} 2
apid_mail_send_failures_total{reason="permanent"} 0
apid_mail_send_failures_total{reason="suppressed"} 0
apid_mail_send_failures_total{reason="transient"} 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected), "apid_mail_send_failures_total"); err != nil {
		t.Error(err)
	}
}

func TestPrometheusMetrics_RecordRetry(t *testing.T) {
	t.Parallel()
	reg := prometheus.NewRegistry()
	m, err := NewPrometheusMetrics(reg, "meterd")
	if err != nil {
		t.Fatalf("NewPrometheusMetrics: %v", err)
	}
	m.RecordRetry(TransportResend)
	if got := testutil.ToFloat64(m.retries.WithLabelValues(TransportResend)); got != 1 {
		t.Errorf("retries[resend] = %v, want 1", got)
	}
}

// Both the nil interface value and a nil concrete receiver must be safe
// so tests and dev wiring can leave the seam unset.
func TestPrometheusMetrics_NilReceiverIsSafe(t *testing.T) {
	t.Parallel()
	var m *PrometheusMetrics
	m.RecordFailure(ReasonTransient)
	m.RecordRetry(TransportResend)

	var zero PrometheusMetrics
	zero.RecordFailure(ReasonTransient)
	zero.RecordRetry(TransportResend)
}

func TestNoopMetrics_SatisfiesInterface(t *testing.T) {
	t.Parallel()
	var m Metrics = NoopMetrics{}
	m.RecordFailure(ReasonPermanent)
	m.RecordRetry(TransportPostmark)
}

// PrometheusMetrics must satisfy Metrics; a compile-time assertion here
// keeps the seam honest if the interface grows.
var _ Metrics = (*PrometheusMetrics)(nil)
var _ Metrics = NoopMetrics{}
