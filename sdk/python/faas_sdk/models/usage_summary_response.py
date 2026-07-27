from __future__ import annotations

from collections.abc import Mapping
from typing import Any, TypeVar

from attrs import define as _attrs_define
from attrs import field as _attrs_field

from ..types import UNSET, Unset

T = TypeVar("T", bound="UsageSummaryResponse")


@_attrs_define
class UsageSummaryResponse:
    """Account-level monthly roll-up: included GB-hours, used, overage math, remaining balance, and informational
    used_cpu_hours (issue #279 / PR-B). The CPU dimension is observable but not yet billed; the GB-hours fields drive
    the overage math.

    """

    month: str
    used_gb_hours: float
    included_gb_hours: int
    overage_gb_hours: float
    overage_cents: int
    """Integer cents. Overages are €0.01/GB-h."""
    used_cpu_hours: float | Unset = UNSET
    """Per-month CPU-hours (informational; not billed). issue #279 / PR-B."""
    additional_properties: dict[str, Any] = _attrs_field(init=False, factory=dict)

    def to_dict(self) -> dict[str, Any]:
        month = self.month

        used_gb_hours = self.used_gb_hours

        included_gb_hours = self.included_gb_hours

        overage_gb_hours = self.overage_gb_hours

        overage_cents = self.overage_cents

        used_cpu_hours = self.used_cpu_hours

        field_dict: dict[str, Any] = {}
        field_dict.update(self.additional_properties)
        field_dict.update(
            {
                "month": month,
                "used_gb_hours": used_gb_hours,
                "included_gb_hours": included_gb_hours,
                "overage_gb_hours": overage_gb_hours,
                "overage_cents": overage_cents,
            }
        )
        if used_cpu_hours is not UNSET:
            field_dict["used_cpu_hours"] = used_cpu_hours

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        month = d.pop("month")

        used_gb_hours = d.pop("used_gb_hours")

        included_gb_hours = d.pop("included_gb_hours")

        overage_gb_hours = d.pop("overage_gb_hours")

        overage_cents = d.pop("overage_cents")

        used_cpu_hours = d.pop("used_cpu_hours", UNSET)

        usage_summary_response = cls(
            month=month,
            used_gb_hours=used_gb_hours,
            included_gb_hours=included_gb_hours,
            overage_gb_hours=overage_gb_hours,
            overage_cents=overage_cents,
            used_cpu_hours=used_cpu_hours,
        )

        usage_summary_response.additional_properties = d
        return usage_summary_response

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
