from __future__ import annotations

from collections.abc import Mapping
from typing import TYPE_CHECKING, Any, TypeVar

from attrs import define as _attrs_define
from attrs import field as _attrs_field

if TYPE_CHECKING:
    from ..models.debug_telemetry_request_item import DebugTelemetryRequestItem


T = TypeVar("T", bound="DebugTelemetryListResponse")


@_attrs_define
class DebugTelemetryListResponse:
    """Response from GET /v1/apps/{slug}/debug/requests (ADR-127).
    `since` echoes the effective window used (after the plan's
    `DebugTelemetryRetentionDays` clamp) so the dashboard can
    surface a "you widened past the cap" tile.

    """

    since: str
    """Effective window applied (e.g. '24h', '72h')."""
    requests: list[DebugTelemetryRequestItem]
    additional_properties: dict[str, Any] = _attrs_field(init=False, factory=dict)

    def to_dict(self) -> dict[str, Any]:
        since = self.since

        requests = []
        for requests_item_data in self.requests:
            requests_item = requests_item_data.to_dict()
            requests.append(requests_item)

        field_dict: dict[str, Any] = {}
        field_dict.update(self.additional_properties)
        field_dict.update(
            {
                "since": since,
                "requests": requests,
            }
        )

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        from ..models.debug_telemetry_request_item import DebugTelemetryRequestItem

        d = dict(src_dict)
        since = d.pop("since")

        requests = []
        _requests = d.pop("requests")
        for requests_item_data in _requests:
            requests_item = DebugTelemetryRequestItem.from_dict(requests_item_data)

            requests.append(requests_item)

        debug_telemetry_list_response = cls(
            since=since,
            requests=requests,
        )

        debug_telemetry_list_response.additional_properties = d
        return debug_telemetry_list_response

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
