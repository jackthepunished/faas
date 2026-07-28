from __future__ import annotations

import datetime
from collections.abc import Mapping
from typing import Any, TypeVar

from attrs import define as _attrs_define
from attrs import field as _attrs_field

from ..models.app_metrics_response_range import AppMetricsResponseRange, check_app_metrics_response_range

T = TypeVar("T", bound="AppMetricsResponse")


@_attrs_define
class AppMetricsResponse:
    """Per-app metrics snapshot (issue #273 / ADR-041). Latencies are
    milliseconds for the 2xx class only; failures surface as
    `error_rate_pct`. `wake_p95_ms` is the FLEET p95 — the
    underlying `gateway_wake_latency_seconds` histogram is
    unlabeled. On Prometheus failure the endpoint returns 200 with
    zeroed fields and `source: "degraded: <reason>"`.

    """

    app_id: str
    range_: AppMetricsResponseRange
    """Echoed window."""
    source: str
    """"prometheus" on success, "degraded: <reason>" otherwise."""
    as_of: datetime.datetime
    """RFC3339Nano UTC."""
    request_count: int
    """Share of requests in the window. Drives the dashboard empty state."""
    latency_p50_ms: float
    """p50 of `gateway_request_duration_seconds_bucket{class="2xx"}` over the window, in ms."""
    latency_p95_ms: float
    """p95 over 2xx traffic in the window, in ms."""
    latency_p99_ms: float
    """p99 over 2xx traffic in the window, in ms."""
    error_rate_pct: float
    """Share of [45]xx requests in the window."""
    cold_start_pct: float
    """Share of requests that triggered a cold boot (the WakeGate
    leader). Followers waiting on the gate see zero cold
    contribution; their wait is on the §12 dashboard via
    `gateway_wake_queue_wait_seconds`.
    """
    wake_p95_ms: float
    """FLEET p95 wake latency (the unlabeled histogram). Labelled as such in the UI."""
    additional_properties: dict[str, Any] = _attrs_field(init=False, factory=dict)

    def to_dict(self) -> dict[str, Any]:
        app_id = self.app_id

        range_: str = self.range_

        source = self.source

        as_of = self.as_of.isoformat()

        request_count = self.request_count

        latency_p50_ms = self.latency_p50_ms

        latency_p95_ms = self.latency_p95_ms

        latency_p99_ms = self.latency_p99_ms

        error_rate_pct = self.error_rate_pct

        cold_start_pct = self.cold_start_pct

        wake_p95_ms = self.wake_p95_ms

        field_dict: dict[str, Any] = {}
        field_dict.update(self.additional_properties)
        field_dict.update(
            {
                "app_id": app_id,
                "range": range_,
                "source": source,
                "as_of": as_of,
                "request_count": request_count,
                "latency_p50_ms": latency_p50_ms,
                "latency_p95_ms": latency_p95_ms,
                "latency_p99_ms": latency_p99_ms,
                "error_rate_pct": error_rate_pct,
                "cold_start_pct": cold_start_pct,
                "wake_p95_ms": wake_p95_ms,
            }
        )

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        app_id = d.pop("app_id")

        range_ = check_app_metrics_response_range(d.pop("range"))

        source = d.pop("source")

        as_of = datetime.datetime.fromisoformat(d.pop("as_of"))

        request_count = d.pop("request_count")

        latency_p50_ms = d.pop("latency_p50_ms")

        latency_p95_ms = d.pop("latency_p95_ms")

        latency_p99_ms = d.pop("latency_p99_ms")

        error_rate_pct = d.pop("error_rate_pct")

        cold_start_pct = d.pop("cold_start_pct")

        wake_p95_ms = d.pop("wake_p95_ms")

        app_metrics_response = cls(
            app_id=app_id,
            range_=range_,
            source=source,
            as_of=as_of,
            request_count=request_count,
            latency_p50_ms=latency_p50_ms,
            latency_p95_ms=latency_p95_ms,
            latency_p99_ms=latency_p99_ms,
            error_rate_pct=error_rate_pct,
            cold_start_pct=cold_start_pct,
            wake_p95_ms=wake_p95_ms,
        )

        app_metrics_response.additional_properties = d
        return app_metrics_response

    @property
    def additional_keys(self) -> list[str]:
        return list(self.additional_properties.keys())

    def __getitem__(self, key: str) -> Any:
        return self.additional_properties[key]

    def __setitem__(self, key: str, value: Any) -> None:
        self.additional_properties[key] = value

    def __delitem__(self, key: str) -> None:
        del self.additional_properties[key]

    def __contains__(self, key: str) -> bool:
        return key in self.additional_properties
