from typing import Literal

UpdateAlertRuleRequestMetric = Literal[
    "cold_start_pct",
    "error_rate_pct",
    "failed_invocations",
    "latency_p50_ms",
    "latency_p95_ms",
    "latency_p99_ms",
    "request_count",
]

UPDATE_ALERT_RULE_REQUEST_METRIC_VALUES: set[UpdateAlertRuleRequestMetric] = {
    "cold_start_pct",
    "error_rate_pct",
    "failed_invocations",
    "latency_p50_ms",
    "latency_p95_ms",
    "latency_p99_ms",
    "request_count",
}


def check_update_alert_rule_request_metric(value: str) -> UpdateAlertRuleRequestMetric:
    if value in UPDATE_ALERT_RULE_REQUEST_METRIC_VALUES:
        return value
    raise TypeError(f"Unexpected value {value!r}. Expected one of {UPDATE_ALERT_RULE_REQUEST_METRIC_VALUES!r}")
