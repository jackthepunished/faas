from __future__ import annotations

from collections.abc import Mapping
from typing import Any, TypeVar

from attrs import define as _attrs_define
from attrs import field as _attrs_field

from ..models.edge_rule_cache_action_methods_item import (
    EdgeRuleCacheActionMethodsItem,
    check_edge_rule_cache_action_methods_item,
)
from ..models.edge_rule_cache_action_vary_on_item import (
    EdgeRuleCacheActionVaryOnItem,
    check_edge_rule_cache_action_vary_on_item,
)
from ..types import UNSET, Unset

T = TypeVar("T", bound="EdgeRuleCacheAction")


@_attrs_define
class EdgeRuleCacheAction:
    """Per-route TTL knobs for safe response caching on selected
    GET/HEAD paths (ADR-122 / §4.1.2.17). The primitive for
    "GET /catalog/* → 60 s fresh + 5 min stale-on-error" without
    forcing the customer to bring their own cache (Upstash
    Redis). The hot-path applier
    (pkg/gateway/handler_apply_edge_rule_cache.go) consults the
    matched rule on each request; a hit serves the cached body
    and returns BEFORE the wake gate, so no VM runs and no
    `gb_ram_hour` accrues.

    Auth posture: requests carrying `Authorization` or a session
    cookie are a hard bypass — they are NEVER stored and NEVER
    served. `vary_on` therefore accepts ONLY
    `Accept-Language` / `Accept-Encoding`; adding
    `Authorization` / `Cookie` to `vary_on` is rejected at
    create-time with 422.

    Field-by-field:
      * `max_age_seconds` — fresh window in seconds. Default 60,
        range `[0, 3600]`. A zero value disables fresh hits but
        stale-on-error still applies within
        `stale_if_error_seconds`.
      * `stale_if_error_seconds` — post-fresh window during
        which a stored entry may be served ONLY on origin
        failure (wake gate failure or upstream 5xx/timeout).
        Default 300, hard cap 300. Stale-while-revalidate
        (serving stale while a refresh runs in the background)
        is NOT supported.
      * `vary_on` — closed vocabulary subset
        (`Accept-Language`, `Accept-Encoding`) whose values
        participate in the cache key. Empty = no vary
        dimension beyond the URL.
      * `methods` — optional method allowlist (default
        `{GET, HEAD}`). Anything outside this set is rejected
        at create-time with 422.

    Per-plan quota: Free 0 (closed), Hobby 1, Pro 5, Scale 20
    (`Limits.EdgeRulesCachePerApp`). The Free=0 default is
    deliberate — the wake-elision guarantee is the upsell, not
    a baseline amenity.

    """

    max_age_seconds: int
    """Fresh window in seconds (default 60, range [0, 3600])."""
    stale_if_error_seconds: int
    """Post-fresh window during which a stored entry may be served ONLY on origin failure (default 300, hard cap
    300)."""
    vary_on: list[EdgeRuleCacheActionVaryOnItem] | Unset = UNSET
    """Closed vocabulary subset (Accept-Language, Accept-Encoding) whose values participate in the cache key."""
    methods: list[EdgeRuleCacheActionMethodsItem] | Unset = UNSET
    """Optional method allowlist (default {GET, HEAD})."""
    additional_properties: dict[str, Any] = _attrs_field(init=False, factory=dict)

    def to_dict(self) -> dict[str, Any]:
        max_age_seconds = self.max_age_seconds

        stale_if_error_seconds = self.stale_if_error_seconds

        vary_on: list[str] | Unset = UNSET
        if not isinstance(self.vary_on, Unset):
            vary_on = []
            for vary_on_item_data in self.vary_on:
                vary_on_item: str = vary_on_item_data
                vary_on.append(vary_on_item)

        methods: list[str] | Unset = UNSET
        if not isinstance(self.methods, Unset):
            methods = []
            for methods_item_data in self.methods:
                methods_item: str = methods_item_data
                methods.append(methods_item)

        field_dict: dict[str, Any] = {}
        field_dict.update(self.additional_properties)
        field_dict.update(
            {
                "max_age_seconds": max_age_seconds,
                "stale_if_error_seconds": stale_if_error_seconds,
            }
        )
        if vary_on is not UNSET:
            field_dict["vary_on"] = vary_on
        if methods is not UNSET:
            field_dict["methods"] = methods

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        max_age_seconds = d.pop("max_age_seconds")

        stale_if_error_seconds = d.pop("stale_if_error_seconds")

        _vary_on = d.pop("vary_on", UNSET)
        vary_on: list[EdgeRuleCacheActionVaryOnItem] | Unset = UNSET
        if _vary_on is not UNSET:
            vary_on = []
            for vary_on_item_data in _vary_on:
                vary_on_item = check_edge_rule_cache_action_vary_on_item(vary_on_item_data)

                vary_on.append(vary_on_item)

        _methods = d.pop("methods", UNSET)
        methods: list[EdgeRuleCacheActionMethodsItem] | Unset = UNSET
        if _methods is not UNSET:
            methods = []
            for methods_item_data in _methods:
                methods_item = check_edge_rule_cache_action_methods_item(methods_item_data)

                methods.append(methods_item)

        edge_rule_cache_action = cls(
            max_age_seconds=max_age_seconds,
            stale_if_error_seconds=stale_if_error_seconds,
            vary_on=vary_on,
            methods=methods,
        )

        edge_rule_cache_action.additional_properties = d
        return edge_rule_cache_action

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
