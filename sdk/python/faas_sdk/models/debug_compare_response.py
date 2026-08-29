from __future__ import annotations

from collections.abc import Mapping
from typing import TYPE_CHECKING, Any, TypeVar
from uuid import UUID

from attrs import define as _attrs_define
from attrs import field as _attrs_field

if TYPE_CHECKING:
    from ..models.debug_compare_route_stats import DebugCompareRouteStats


T = TypeVar("T", bound="DebugCompareResponse")


@_attrs_define
class DebugCompareResponse:
    """POST response from /v1/apps/{slug}/debug/compare (ADR-127 / PR-B)."""

    source: UUID
    mirror: UUID
    routes: list[DebugCompareRouteStats]
    additional_properties: dict[str, Any] = _attrs_field(init=False, factory=dict)

    def to_dict(self) -> dict[str, Any]:
        source = str(self.source)

        mirror = str(self.mirror)

        routes = []
        for routes_item_data in self.routes:
            routes_item = routes_item_data.to_dict()
            routes.append(routes_item)

        field_dict: dict[str, Any] = {}
        field_dict.update(self.additional_properties)
        field_dict.update(
            {
                "source": source,
                "mirror": mirror,
                "routes": routes,
            }
        )

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        from ..models.debug_compare_route_stats import DebugCompareRouteStats

        d = dict(src_dict)
        source = UUID(d.pop("source"))

        mirror = UUID(d.pop("mirror"))

        routes = []
        _routes = d.pop("routes")
        for routes_item_data in _routes:
            routes_item = DebugCompareRouteStats.from_dict(routes_item_data)

            routes.append(routes_item)

        debug_compare_response = cls(
            source=source,
            mirror=mirror,
            routes=routes,
        )

        debug_compare_response.additional_properties = d
        return debug_compare_response

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
