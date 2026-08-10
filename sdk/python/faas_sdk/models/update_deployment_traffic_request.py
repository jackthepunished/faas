from __future__ import annotations

from collections.abc import Mapping
from typing import Any, TypeVar

from attrs import define as _attrs_define
from attrs import field as _attrs_field

T = TypeVar("T", bound="UpdateDeploymentTrafficRequest")


@_attrs_define
class UpdateDeploymentTrafficRequest:
    """Body for PATCH /v1/deployments/{id}/traffic (issue #556 PR-A). Sets the per-deployment traffic-split weight (integer
    [0, 100]). PR-A uses the zero-siblings rebalance form: setting row R's traffic_percent to N forces every other live
    row in the same app to 0, keeping Σ = 100 by construction. Pro/Scale only — Free/Hobby are rejected at 403
    plan_traffic_split_not_allowed.

    """

    traffic_percent: int
    """Per-deployment traffic-split weight. 0 = no traffic (used during rollback). 100 = sole live deployment."""
    additional_properties: dict[str, Any] = _attrs_field(init=False, factory=dict)

    def to_dict(self) -> dict[str, Any]:
        traffic_percent = self.traffic_percent

        field_dict: dict[str, Any] = {}
        field_dict.update(self.additional_properties)
        field_dict.update(
            {
                "traffic_percent": traffic_percent,
            }
        )

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        traffic_percent = d.pop("traffic_percent")

        update_deployment_traffic_request = cls(
            traffic_percent=traffic_percent,
        )

        update_deployment_traffic_request.additional_properties = d
        return update_deployment_traffic_request

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
