from __future__ import annotations

from collections.abc import Mapping
from typing import Any, TypeVar, cast

from attrs import define as _attrs_define
from attrs import field as _attrs_field

from ..models.app_routes_response_source import AppRoutesResponseSource, check_app_routes_response_source
from ..types import UNSET, Unset

T = TypeVar("T", bound="AppRoutesResponse")


@_attrs_define
class AppRoutesResponse:
    """Per-route label snapshot (ADR-093). The bounded route label
    set the gatewayd-internal control listener emits for the app.
    Each item is `"<METHOD> <PATH>"` (pre-edge-rule-rewrite) for
    an admitted route, or the reserved `"__route_other__"` overflow
    bucket label. Bounded at 50 distinct real routes + the reserved
    overflow per app (ADR-093 D2).

    """

    slug: str
    routes: list[str]
    source: AppRoutesResponseSource
    app_id: str | Unset = UNSET
    additional_properties: dict[str, Any] = _attrs_field(init=False, factory=dict)

    def to_dict(self) -> dict[str, Any]:
        slug = self.slug

        routes = self.routes

        source: str = self.source

        app_id = self.app_id

        field_dict: dict[str, Any] = {}
        field_dict.update(self.additional_properties)
        field_dict.update(
            {
                "slug": slug,
                "routes": routes,
                "source": source,
            }
        )
        if app_id is not UNSET:
            field_dict["app_id"] = app_id

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        slug = d.pop("slug")

        routes = cast(list[str], d.pop("routes"))

        source = check_app_routes_response_source(d.pop("source"))

        app_id = d.pop("app_id", UNSET)

        app_routes_response = cls(
            slug=slug,
            routes=routes,
            source=source,
            app_id=app_id,
        )

        app_routes_response.additional_properties = d
        return app_routes_response

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
