from typing import Literal

ScalingTargetMetric = Literal["concurrent_requests", "p99_latency_ms", "rps"]

SCALING_TARGET_METRIC_VALUES: set[ScalingTargetMetric] = {
    "concurrent_requests",
    "p99_latency_ms",
    "rps",
}


def check_scaling_target_metric(value: str) -> ScalingTargetMetric:
    if value in SCALING_TARGET_METRIC_VALUES:
        return value
    raise TypeError(f"Unexpected value {value!r}. Expected one of {SCALING_TARGET_METRIC_VALUES!r}")
