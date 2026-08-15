// Package reqbudget: metrics.go — Prometheus metric registration.
//
// Four metric families total — two per daemon (gatewayd-public,
// apid) — registered via MustRegister against a caller-supplied
// *prometheus.Registry. Both daemons construct one M struct at
// startup and pass it into BudgetMiddleware; the metric names match
// the §12 implementation spec convention (gateway_*, apid_*).
//
// Naming follows the project convention: counter names end in
// _total; histograms name the thing being measured without a
// suffix; bucket boundaries are aligned with the project's existing
// gateway_wake_latency_seconds / apid_request_duration_seconds
// shapes so dashboards don't need rebucketing.
package reqbudget

import (
	"github.com/prometheus/client_golang/prometheus"
)

// M is the metric set a daemon wires into its BudgetMiddleware. The
// zero value is NOT usable — callers must construct via NewMetrics
// (which registers against a registry) and pass M by value.
//
// Field order matches metric-name alphabetical sort so gofmt
// doesn't shuffle unrelated struct literals.
type M struct {
	// RequestBudgetSeconds is the histogram of remaining-budget at
	// deadline fire (label outcome=exceeded|cancelled) or at
	// successful handler return (outcome=set). Bucket boundaries
	// are aligned with the gateway wake-latency buckets so a single
	// Grafana panel can show both.
	RequestBudgetSeconds *prometheus.HistogramVec
	// RequestBudgetExceededTotal counts deadline fires attributed to
	// a particular downstream hop (gateway|jwt|forward|stream|db|http).
	// Routinely non-zero on the long tail; spikes correlate with
	// downstream saturation.
	RequestBudgetExceededTotal *prometheus.CounterVec
}

// NewMetrics constructs the four histograms/counters and registers
// them against reg. registryNamespace distinguishes gateway from
// apid (so a single shared binary that mounts both won't double-
// register — though today's deployment runs each daemon in its own
// process, the namespace knob is cheap insurance).
//
// registryNamespace must be one of "gateway", "apid". Any other
// value is rejected up front so a typo doesn't land as a silent
// no-op against DefaultRegisterer.
func NewMetrics(reg prometheus.Registerer, registryNamespace string) (*M, error) {
	switch registryNamespace {
	case "gateway", "apid":
	default:
		return nil, errBadNamespace(registryNamespace)
	}
	m := &M{
		RequestBudgetSeconds: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: registryNamespace,
			Name:      "request_budget_seconds",
			Help:      "Remaining-budget time at outcome: set=handler-success, exceeded=deadline-fired, cancelled=client-disconnected.",
			Buckets:   []float64{0.005, 0.025, 0.1, 0.5, 1, 2, 5, 10, 30},
		}, []string{"route", "endpoint", "outcome"}),
		RequestBudgetExceededTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: registryNamespace,
			Name:      "request_budget_exceeded_total",
			Help:      "Count of deadline fires attributed to a particular downstream hop.",
		}, []string{"route", "endpoint", "hop"}),
	}
	if err := reg.Register(m.RequestBudgetSeconds); err != nil {
		return nil, err
	}
	if err := reg.Register(m.RequestBudgetExceededTotal); err != nil {
		return nil, err
	}
	return m, nil
}

// errBadNamespace is returned when NewMetrics gets an unknown
// registryNamespace. Kept inline (not a sentinel) because callers
// can't usefully branch on the value — they must fix the typo.
type badNamespaceError string

func (e badNamespaceError) Error() string {
	return "reqbudget: bad registry namespace " + string(e) + ` (must be "gateway" or "apid")`
}

func errBadNamespace(s string) error { return badNamespaceError(s) }
