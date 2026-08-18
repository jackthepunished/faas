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

    `cap_hit` (ADR-093 Tier B item #1) is true iff the app's
    route label set has reached `RouteMetricsPerAppCap` (50) and
    additional routes are collapsing into the reserved
    `__route_other__` overflow bucket. When `cap_hit` is true,
    `len(routes) == RouteMetricsPerAppCap + 2` (50 real + the
    reserved empty label + `__route_other__`). When false, the
    dashboard can render "you have N admitted routes" without
    having to count the array (which is ambiguous: 5 real routes
    + `__route_other__` is indistinguishable from 50 real routes
    + overflow). Omitted on the `source: unavailable` path —
    the gatewayd-internal dial failed, the cap state is unknown.

    """

    slug: str
    routes: list[str]
    source: AppRoutesResponseSource
    app_id: str | Unset = UNSET
    cap_hit: bool | Unset = False
    """True iff the route label set has hit `RouteMetricsPerAppCap`
    (50) and additional routes are collapsing into
    `__route_other__`. Omitted on `source: unavailable` paths
    (cap state is unknown when the gatewayd-internal dial
    fails).
    """
    additional_properties: dict[str, Any] = _attrs_field(init=False, factory=dict)

    def to_dict(self) -> dict[str, Any]:
        slug = self.slug

        routes = self.routes

        source: str = self.source

        app_id = self.app_id

        cap_hit = self.cap_hit

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
        if cap_hit is not UNSET:
            field_dict["cap_hit"] = cap_hit

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        slug = d.pop("slug")

        routes = cast(list[str], d.pop("routes"))

        source = check_app_routes_response_source(d.pop("source"))

        app_id = d.pop("app_id", UNSET)

        cap_hit = d.pop("cap_hit", UNSET)

        app_routes_response = cls(
            slug=slug,
            routes=routes,
            source=source,
            app_id=app_id,
            cap_hit=cap_hit,
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
