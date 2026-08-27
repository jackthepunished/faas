from __future__ import annotations

from collections.abc import Mapping
from typing import Any, TypeVar, cast

from attrs import define as _attrs_define
from attrs import field as _attrs_field

from ..models.canary_preset_spec_preset import CanaryPresetSpecPreset, check_canary_preset_spec_preset
from ..types import UNSET, Unset

T = TypeVar("T", bound="CanaryPresetSpec")


@_attrs_define
class CanaryPresetSpec:
    """Progressive canary rollout preset requested on deployment creation (issue #976 / ADR-122)."""

    preset: CanaryPresetSpecPreset
    """Closed-set canary ladder catalog name."""
    step_durations: list[int] | Unset = UNSET
    """Optional step durations encoded as Go time.Duration nanoseconds; reserved for custom-ladder support."""
    additional_properties: dict[str, Any] = _attrs_field(init=False, factory=dict)

    def to_dict(self) -> dict[str, Any]:
        preset: str = self.preset

        step_durations: list[int] | Unset = UNSET
        if not isinstance(self.step_durations, Unset):
            step_durations = self.step_durations

        field_dict: dict[str, Any] = {}
        field_dict.update(self.additional_properties)
        field_dict.update(
            {
                "preset": preset,
            }
        )
        if step_durations is not UNSET:
            field_dict["step_durations"] = step_durations

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        preset = check_canary_preset_spec_preset(d.pop("preset"))

        step_durations = cast(list[int], d.pop("step_durations", UNSET))

        canary_preset_spec = cls(
            preset=preset,
            step_durations=step_durations,
        )

        canary_preset_spec.additional_properties = d
        return canary_preset_spec

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
