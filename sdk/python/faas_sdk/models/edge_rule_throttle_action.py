from __future__ import annotations

from collections.abc import Mapping
from typing import Any, TypeVar

from attrs import define as _attrs_define
from attrs import field as _attrs_field

T = TypeVar("T", bound="EdgeRuleThrottleAction")


@_attrs_define
class EdgeRuleThrottleAction:
    """Per-route token-bucket cap (ADR-091 D20.5 amendment,
    issue #881). Customers tighten the per-route rps/burst
    below their plan's `plan.RateLimitRPS` — the apid
    validator enforces the sub-plan ceiling; the gateway
    compiler enforces it again at load time.

    Sub-plan ceiling — the load-bearing constraint. A
    throttle rule is STRICTLY a tightening primitive. A
    rule that exceeds the ceiling is rejected with 422
    BEFORE any DB write — a customer cannot raise their
    plan limit by registering a throttle rule.

    Per-IP sub-keying is deliberately absent in v1 — see
    the package doc on `pkg/state.EdgeRuleThrottleAction`
    for the design rationale (memory-bounded limiter +
    attacker-controlled IP cardinality = unbounded bucket
    growth).

    """

    requests_per_second: float
    """Token-bucket refill rate per route. The apid
    validator rejects rps > plan.RateLimitRPS with a
    422. The gateway compiler clamps + warns on the
    same bound at load time.
    """
    burst: int
    """Token-bucket burst per route. Mirrors rps: rejected
    above `plan.RateLimitBurst` at create time and
    clamped at compile time.
    """
    additional_properties: dict[str, Any] = _attrs_field(init=False, factory=dict)

    def to_dict(self) -> dict[str, Any]:
        requests_per_second = self.requests_per_second

        burst = self.burst

        field_dict: dict[str, Any] = {}
        field_dict.update(self.additional_properties)
        field_dict.update(
            {
                "requests_per_second": requests_per_second,
                "burst": burst,
            }
        )

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        requests_per_second = d.pop("requests_per_second")

        burst = d.pop("burst")

        edge_rule_throttle_action = cls(
            requests_per_second=requests_per_second,
            burst=burst,
        )

        edge_rule_throttle_action.additional_properties = d
        return edge_rule_throttle_action

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
