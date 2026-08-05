from __future__ import annotations

from collections.abc import Mapping
from typing import TYPE_CHECKING, Any, TypeVar, cast

from attrs import define as _attrs_define
from attrs import field as _attrs_field

from ..models.update_app_request_eviction_priority_type_1 import (
    UpdateAppRequestEvictionPriorityType1,
    check_update_app_request_eviction_priority_type_1,
)
from ..models.update_app_request_eviction_priority_type_2_type_1 import (
    UpdateAppRequestEvictionPriorityType2Type1,
    check_update_app_request_eviction_priority_type_2_type_1,
)
from ..models.update_app_request_eviction_priority_type_3_type_1 import (
    UpdateAppRequestEvictionPriorityType3Type1,
    check_update_app_request_eviction_priority_type_3_type_1,
)
from ..types import UNSET, Unset

if TYPE_CHECKING:
    from ..models.scaling_policy import ScalingPolicy


T = TypeVar("T", bound="UpdateAppRequest")


@_attrs_define
class UpdateAppRequest:
    """Partial update — every field is optional; omitted fields are unchanged."""

    ram_mb: int | None | Unset = UNSET
    idle_timeout_s: int | None | Unset = UNSET
    max_concurrency: int | None | Unset = UNSET
    min_instances: int | None | Unset = UNSET
    egress_allowlist: list[str] | Unset = UNSET
    """v4 or v6 CIDR allowlist; empty array clears to chain-default-accept."""
    autoscale_target_rps: int | None | Unset = UNSET
    """Per-instance RPS target for the reactive scale-up trigger. 0 = disable. Hobby/Pro/Scale only. Values < 0 are
    422 invalid_autoscale_target_rps."""
    autoscale_target_cpu_pct: int | None | Unset = UNSET
    """Per-instance CPU% target (1..100, 0 = disable) for the reactive scale-up trigger. Pro/Scale only. Values
    outside [1, 100] (other than 0) are 422 invalid_autoscale_target_cpu_pct."""
    streaming_enabled: bool | None | Unset = UNSET
    """Per-app streaming flag (issue #471). Omitted → no change. Free PATCHing true is 403
    plan_streaming_not_allowed."""
    scaling_policy: None | ScalingPolicy | Unset = UNSET
    """Per-app scaling policy. Omitted → no change. Non-null → atomic full-overwrite of the jsonb column."""
    require_signed: bool | None | Unset = UNSET
    """DEPRECATED on this surface. The customer PATCH /v1/apps/{slug} endpoint silently drops require_signed; the
    operator endpoint PATCH /v1/apps/{slug}/security is the only path that flips the flag (issue #472 / ADR-054).
    The field is parsed for backwards compatibility but never persisted from this endpoint."""
    warm_snapshot_enabled: bool | None | Unset = UNSET
    """Per-app two-tier snapshot flag (issue #470 / ADR-055). Omitted → no change. PATCH-true on Free/Hobby is
    rejected with 403 plan_warm_snapshot_not_allowed."""
    warm_snapshot_min_requests: int | None | Unset = UNSET
    """Per-app request-count threshold for warm-tier capture (issue #470 / ADR-055). Range [1, 100]. Omitted → no
    change."""
    warm_snapshot_min_ms: int | None | Unset = UNSET
    """Per-app time-since-first-ready threshold for warm-tier capture, milliseconds (issue #470 / ADR-055). Range
    [100, 60000]. Omitted → no change."""
    eviction_priority: (
        None
        | Unset
        | UpdateAppRequestEvictionPriorityType1
        | UpdateAppRequestEvictionPriorityType2Type1
        | UpdateAppRequestEvictionPriorityType3Type1
    ) = UNSET
    """Per-app eviction tier (issue #475). 'best_effort' (default) keeps the pre-#475 LRU-by-last_request_at reaper
    behaviour; 'reserved' protects the app from cross-account RAM-pressure eviction (every best_effort candidate is
    drained before any reserved is parked). Plan-gated upstream: Free PATCH 'reserved' returns 402
    plan_eviction_priority_reserved_not_allowed. Per-account cap (Hobby 1, Pro 2, Scale 4): 422
    plan_eviction_priority_reserved_quota when exhausted. Omitted → no change."""
    require_authn: bool | None | Unset = UNSET
    """Per-deployment token-gate flag (issue #560). Omitted → no change. PATCH-true on Free/Hobby is rejected with
    403 plan_require_authn_not_allowed."""
    additional_properties: dict[str, Any] = _attrs_field(init=False, factory=dict)

    def to_dict(self) -> dict[str, Any]:
        from ..models.scaling_policy import ScalingPolicy

        ram_mb: int | None | Unset
        if isinstance(self.ram_mb, Unset):
            ram_mb = UNSET
        else:
            ram_mb = self.ram_mb

        idle_timeout_s: int | None | Unset
        if isinstance(self.idle_timeout_s, Unset):
            idle_timeout_s = UNSET
        else:
            idle_timeout_s = self.idle_timeout_s

        max_concurrency: int | None | Unset
        if isinstance(self.max_concurrency, Unset):
            max_concurrency = UNSET
        else:
            max_concurrency = self.max_concurrency

        min_instances: int | None | Unset
        if isinstance(self.min_instances, Unset):
            min_instances = UNSET
        else:
            min_instances = self.min_instances

        egress_allowlist: list[str] | Unset = UNSET
        if not isinstance(self.egress_allowlist, Unset):
            egress_allowlist = self.egress_allowlist

        autoscale_target_rps: int | None | Unset
        if isinstance(self.autoscale_target_rps, Unset):
            autoscale_target_rps = UNSET
        else:
            autoscale_target_rps = self.autoscale_target_rps

        autoscale_target_cpu_pct: int | None | Unset
        if isinstance(self.autoscale_target_cpu_pct, Unset):
            autoscale_target_cpu_pct = UNSET
        else:
            autoscale_target_cpu_pct = self.autoscale_target_cpu_pct

        streaming_enabled: bool | None | Unset
        if isinstance(self.streaming_enabled, Unset):
            streaming_enabled = UNSET
        else:
            streaming_enabled = self.streaming_enabled

        scaling_policy: dict[str, Any] | None | Unset
        if isinstance(self.scaling_policy, Unset):
            scaling_policy = UNSET
        elif isinstance(self.scaling_policy, ScalingPolicy):
            scaling_policy = self.scaling_policy.to_dict()
        else:
            scaling_policy = self.scaling_policy

        require_signed: bool | None | Unset
        if isinstance(self.require_signed, Unset):
            require_signed = UNSET
        else:
            require_signed = self.require_signed

        warm_snapshot_enabled: bool | None | Unset
        if isinstance(self.warm_snapshot_enabled, Unset):
            warm_snapshot_enabled = UNSET
        else:
            warm_snapshot_enabled = self.warm_snapshot_enabled

        warm_snapshot_min_requests: int | None | Unset
        if isinstance(self.warm_snapshot_min_requests, Unset):
            warm_snapshot_min_requests = UNSET
        else:
            warm_snapshot_min_requests = self.warm_snapshot_min_requests

        warm_snapshot_min_ms: int | None | Unset
        if isinstance(self.warm_snapshot_min_ms, Unset):
            warm_snapshot_min_ms = UNSET
        else:
            warm_snapshot_min_ms = self.warm_snapshot_min_ms

        eviction_priority: None | str | Unset
        if isinstance(self.eviction_priority, Unset):
            eviction_priority = UNSET
        elif isinstance(self.eviction_priority, str):
            eviction_priority = self.eviction_priority
        elif isinstance(self.eviction_priority, str):
            eviction_priority = self.eviction_priority
        elif isinstance(self.eviction_priority, str):
            eviction_priority = self.eviction_priority
        else:
            eviction_priority = self.eviction_priority

        require_authn: bool | None | Unset
        if isinstance(self.require_authn, Unset):
            require_authn = UNSET
        else:
            require_authn = self.require_authn

        field_dict: dict[str, Any] = {}
        field_dict.update(self.additional_properties)
        field_dict.update({})
        if ram_mb is not UNSET:
            field_dict["ram_mb"] = ram_mb
        if idle_timeout_s is not UNSET:
            field_dict["idle_timeout_s"] = idle_timeout_s
        if max_concurrency is not UNSET:
            field_dict["max_concurrency"] = max_concurrency
        if min_instances is not UNSET:
            field_dict["min_instances"] = min_instances
        if egress_allowlist is not UNSET:
            field_dict["egress_allowlist"] = egress_allowlist
        if autoscale_target_rps is not UNSET:
            field_dict["autoscale_target_rps"] = autoscale_target_rps
        if autoscale_target_cpu_pct is not UNSET:
            field_dict["autoscale_target_cpu_pct"] = autoscale_target_cpu_pct
        if streaming_enabled is not UNSET:
            field_dict["streaming_enabled"] = streaming_enabled
        if scaling_policy is not UNSET:
            field_dict["scaling_policy"] = scaling_policy
        if require_signed is not UNSET:
            field_dict["require_signed"] = require_signed
        if warm_snapshot_enabled is not UNSET:
            field_dict["warm_snapshot_enabled"] = warm_snapshot_enabled
        if warm_snapshot_min_requests is not UNSET:
            field_dict["warm_snapshot_min_requests"] = warm_snapshot_min_requests
        if warm_snapshot_min_ms is not UNSET:
            field_dict["warm_snapshot_min_ms"] = warm_snapshot_min_ms
        if eviction_priority is not UNSET:
            field_dict["eviction_priority"] = eviction_priority
        if require_authn is not UNSET:
            field_dict["require_authn"] = require_authn

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        from ..models.scaling_policy import ScalingPolicy

        d = dict(src_dict)

        def _parse_ram_mb(data: object) -> int | None | Unset:
            if data is None:
                return data
            if isinstance(data, Unset):
                return data
            return cast(int | None | Unset, data)

        ram_mb = _parse_ram_mb(d.pop("ram_mb", UNSET))

        def _parse_idle_timeout_s(data: object) -> int | None | Unset:
            if data is None:
                return data
            if isinstance(data, Unset):
                return data
            return cast(int | None | Unset, data)

        idle_timeout_s = _parse_idle_timeout_s(d.pop("idle_timeout_s", UNSET))

        def _parse_max_concurrency(data: object) -> int | None | Unset:
            if data is None:
                return data
            if isinstance(data, Unset):
                return data
            return cast(int | None | Unset, data)

        max_concurrency = _parse_max_concurrency(d.pop("max_concurrency", UNSET))

        def _parse_min_instances(data: object) -> int | None | Unset:
            if data is None:
                return data
            if isinstance(data, Unset):
                return data
            return cast(int | None | Unset, data)

        min_instances = _parse_min_instances(d.pop("min_instances", UNSET))

        egress_allowlist = cast(list[str], d.pop("egress_allowlist", UNSET))

        def _parse_autoscale_target_rps(data: object) -> int | None | Unset:
            if data is None:
                return data
            if isinstance(data, Unset):
                return data
            return cast(int | None | Unset, data)

        autoscale_target_rps = _parse_autoscale_target_rps(d.pop("autoscale_target_rps", UNSET))

        def _parse_autoscale_target_cpu_pct(data: object) -> int | None | Unset:
            if data is None:
                return data
            if isinstance(data, Unset):
                return data
            return cast(int | None | Unset, data)

        autoscale_target_cpu_pct = _parse_autoscale_target_cpu_pct(d.pop("autoscale_target_cpu_pct", UNSET))

        def _parse_streaming_enabled(data: object) -> bool | None | Unset:
            if data is None:
                return data
            if isinstance(data, Unset):
                return data
            return cast(bool | None | Unset, data)

        streaming_enabled = _parse_streaming_enabled(d.pop("streaming_enabled", UNSET))

        def _parse_scaling_policy(data: object) -> None | ScalingPolicy | Unset:
            if data is None:
                return data
            if isinstance(data, Unset):
                return data
            try:
                if not isinstance(data, dict):
                    raise TypeError()
                scaling_policy_type_1 = ScalingPolicy.from_dict(data)

                return scaling_policy_type_1
            except (TypeError, ValueError, AttributeError, KeyError):
                pass
            return cast(None | ScalingPolicy | Unset, data)

        scaling_policy = _parse_scaling_policy(d.pop("scaling_policy", UNSET))

        def _parse_require_signed(data: object) -> bool | None | Unset:
            if data is None:
                return data
            if isinstance(data, Unset):
                return data
            return cast(bool | None | Unset, data)

        require_signed = _parse_require_signed(d.pop("require_signed", UNSET))

        def _parse_warm_snapshot_enabled(data: object) -> bool | None | Unset:
            if data is None:
                return data
            if isinstance(data, Unset):
                return data
            return cast(bool | None | Unset, data)

        warm_snapshot_enabled = _parse_warm_snapshot_enabled(d.pop("warm_snapshot_enabled", UNSET))

        def _parse_warm_snapshot_min_requests(data: object) -> int | None | Unset:
            if data is None:
                return data
            if isinstance(data, Unset):
                return data
            return cast(int | None | Unset, data)

        warm_snapshot_min_requests = _parse_warm_snapshot_min_requests(d.pop("warm_snapshot_min_requests", UNSET))

        def _parse_warm_snapshot_min_ms(data: object) -> int | None | Unset:
            if data is None:
                return data
            if isinstance(data, Unset):
                return data
            return cast(int | None | Unset, data)

        warm_snapshot_min_ms = _parse_warm_snapshot_min_ms(d.pop("warm_snapshot_min_ms", UNSET))

        def _parse_eviction_priority(
            data: object,
        ) -> (
            None
            | Unset
            | UpdateAppRequestEvictionPriorityType1
            | UpdateAppRequestEvictionPriorityType2Type1
            | UpdateAppRequestEvictionPriorityType3Type1
        ):
            if data is None:
                return data
            if isinstance(data, Unset):
                return data
            try:
                if not isinstance(data, str):
                    raise TypeError()
                eviction_priority_type_1 = check_update_app_request_eviction_priority_type_1(data)

                return eviction_priority_type_1
            except (TypeError, ValueError, AttributeError, KeyError):
                pass
            try:
                if not isinstance(data, str):
                    raise TypeError()
                eviction_priority_type_2_type_1 = check_update_app_request_eviction_priority_type_2_type_1(data)

                return eviction_priority_type_2_type_1
            except (TypeError, ValueError, AttributeError, KeyError):
                pass
            try:
                if not isinstance(data, str):
                    raise TypeError()
                eviction_priority_type_3_type_1 = check_update_app_request_eviction_priority_type_3_type_1(data)

                return eviction_priority_type_3_type_1
            except (TypeError, ValueError, AttributeError, KeyError):
                pass
            return cast(
                None
                | Unset
                | UpdateAppRequestEvictionPriorityType1
                | UpdateAppRequestEvictionPriorityType2Type1
                | UpdateAppRequestEvictionPriorityType3Type1,
                data,
            )

        eviction_priority = _parse_eviction_priority(d.pop("eviction_priority", UNSET))

        def _parse_require_authn(data: object) -> bool | None | Unset:
            if data is None:
                return data
            if isinstance(data, Unset):
                return data
            return cast(bool | None | Unset, data)

        require_authn = _parse_require_authn(d.pop("require_authn", UNSET))

        update_app_request = cls(
            ram_mb=ram_mb,
            idle_timeout_s=idle_timeout_s,
            max_concurrency=max_concurrency,
            min_instances=min_instances,
            egress_allowlist=egress_allowlist,
            autoscale_target_rps=autoscale_target_rps,
            autoscale_target_cpu_pct=autoscale_target_cpu_pct,
            streaming_enabled=streaming_enabled,
            scaling_policy=scaling_policy,
            require_signed=require_signed,
            warm_snapshot_enabled=warm_snapshot_enabled,
            warm_snapshot_min_requests=warm_snapshot_min_requests,
            warm_snapshot_min_ms=warm_snapshot_min_ms,
            eviction_priority=eviction_priority,
            require_authn=require_authn,
        )

        update_app_request.additional_properties = d
        return update_app_request

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
