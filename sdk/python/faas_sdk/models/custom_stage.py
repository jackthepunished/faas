from __future__ import annotations

from collections.abc import Mapping
from typing import Any, TypeVar

from attrs import define as _attrs_define
from attrs import field as _attrs_field

T = TypeVar("T", bound="CustomStage")


@_attrs_define
class CustomStage:
    """One stage of a customer-supplied canary ladder
    (issue #976 / ADR-122 / SAFE-RELEASES production-leveling
    Stream F). Percent is the traffic share this stage moves
    to (0..100, terminal stage must be 100). Duration is the
    wall-clock dwell time at this stage in time.ParseDuration
    form (e.g. "30s", "2m", "0s" for the terminal hop).

    """

    percent: int
    """Traffic share this stage moves to (0..100). The terminal stage must be 100."""
    duration: str
    """Wall-clock dwell at this stage, in time.ParseDuration form (e.g. '30s', '2m'). '0s' for the terminal hop."""
    additional_properties: dict[str, Any] = _attrs_field(init=False, factory=dict)

    def to_dict(self) -> dict[str, Any]:
        percent = self.percent

        duration = self.duration

        field_dict: dict[str, Any] = {}
        field_dict.update(self.additional_properties)
        field_dict.update(
            {
                "percent": percent,
                "duration": duration,
            }
        )

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        percent = d.pop("percent")

        duration = d.pop("duration")

        custom_stage = cls(
            percent=percent,
            duration=duration,
        )

        custom_stage.additional_properties = d
        return custom_stage

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
