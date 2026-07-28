from typing import Literal

CreateAlertRuleRequestMetric = Literal[
    "cold_start_pct",
    "error_rate_pct",
    "failed_invocations",
    "latency_p50_ms",
    "latency_p95_ms",
    "latency_p99_ms",
    "request_count",
]

CREATE_ALERT_RULE_REQUEST_METRIC_VALUES: set[CreateAlertRuleRequestMetric] = {
    "cold_start_pct",
    "error_rate_pct",
    "failed_invocations",
    "latency_p50_ms",
    "latency_p95_ms",
    "latency_p99_ms",
    "request_count",
}


def check_create_alert_rule_request_metric(value: str) -> CreateAlertRuleRequestMetric:
    if value in CREATE_ALERT_RULE_REQUEST_METRIC_VALUES:
        return value
    raise TypeError(f"Unexpected value {value!r}. Expected one of {CREATE_ALERT_RULE_REQUEST_METRIC_VALUES!r}")
