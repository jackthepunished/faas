from typing import Literal

AlertRuleResponseMetric = Literal[
    "cold_start_pct",
    "error_rate_pct",
    "failed_invocations",
    "latency_p50_ms",
    "latency_p95_ms",
    "latency_p99_ms",
    "request_count",
]

ALERT_RULE_RESPONSE_METRIC_VALUES: set[AlertRuleResponseMetric] = {
    "cold_start_pct",
    "error_rate_pct",
    "failed_invocations",
    "latency_p50_ms",
    "latency_p95_ms",
    "latency_p99_ms",
    "request_count",
}


def check_alert_rule_response_metric(value: str) -> AlertRuleResponseMetric:
    if value in ALERT_RULE_RESPONSE_METRIC_VALUES:
        return value
    raise TypeError(f"Unexpected value {value!r}. Expected one of {ALERT_RULE_RESPONSE_METRIC_VALUES!r}")
