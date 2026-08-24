from __future__ import annotations

from collections.abc import Mapping
from typing import Any, TypeVar

from attrs import define as _attrs_define
from attrs import field as _attrs_field

from ..types import UNSET, Unset

T = TypeVar("T", bound="EnableAlertPresetRequest")


@_attrs_define
class EnableAlertPresetRequest:
    """Body for POST /v1/apps/{slug}/alert-presets/{name}/enable.
    The (name, metric, comparison, threshold, window_spec,
    default_cooldown_minutes) sextuple is pre-filled from the
    catalog; the caller supplies only the delivery-side fields.

    """

    webhook_url: str
    webhook_secret: str
    cooldown_minutes: int | Unset = UNSET
    """Override for the preset's default_cooldown_minutes.
    Omit to use the catalog default.
    """
    enabled: bool | Unset = True
    """Whether the instantiated rule is enabled. Defaults to
    true; pass false to stage the rule in disabled state.
    """
    additional_properties: dict[str, Any] = _attrs_field(init=False, factory=dict)

    def to_dict(self) -> dict[str, Any]:
        webhook_url = self.webhook_url

        webhook_secret = self.webhook_secret

        cooldown_minutes = self.cooldown_minutes

        enabled = self.enabled

        field_dict: dict[str, Any] = {}
        field_dict.update(self.additional_properties)
        field_dict.update(
            {
                "webhook_url": webhook_url,
                "webhook_secret": webhook_secret,
            }
        )
        if cooldown_minutes is not UNSET:
            field_dict["cooldown_minutes"] = cooldown_minutes
        if enabled is not UNSET:
            field_dict["enabled"] = enabled

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        webhook_url = d.pop("webhook_url")

        webhook_secret = d.pop("webhook_secret")

        cooldown_minutes = d.pop("cooldown_minutes", UNSET)

        enabled = d.pop("enabled", UNSET)

        enable_alert_preset_request = cls(
            webhook_url=webhook_url,
            webhook_secret=webhook_secret,
            cooldown_minutes=cooldown_minutes,
            enabled=enabled,
        )

        enable_alert_preset_request.additional_properties = d
        return enable_alert_preset_request

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
