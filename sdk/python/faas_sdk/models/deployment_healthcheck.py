from __future__ import annotations

from collections.abc import Mapping
from typing import Any, TypeVar

from attrs import define as _attrs_define
from attrs import field as _attrs_field

from ..types import UNSET, Unset

T = TypeVar("T", bound="DeploymentHealthcheck")


@_attrs_define
class DeploymentHealthcheck:
    """Readiness-probe shape on the deploy-time override object (issue #460 /
    ADR-053). Today the probe stays a bare TCP accept — `path`, `interval_s`,
    `timeout_s`, `retries` are persisted but not yet exercised by `vmm.waitReady`.

    Validation rules (enforced in `pkg/api/dto.go::CreateDeploymentOverrides.Validate`):
    - `path` must start with `/`.
    - `interval_s`, `timeout_s`, `retries` must be `>= 0`.
    - Missing fields default to 0 (interpreted as "use image default" by the
      future probe implementation).

    """

    path: str
    """Path the probe requests from the guest; must start with `/` (e.g. `/healthz`)."""
    interval_s: int | Unset = UNSET
    """Probe interval in seconds; 0 = use image default."""
    timeout_s: int | Unset = UNSET
    """Probe timeout in seconds; 0 = use image default."""
    retries: int | Unset = UNSET
    """Consecutive failures before the instance is considered unhealthy; 0 = use image default."""
    additional_properties: dict[str, Any] = _attrs_field(init=False, factory=dict)

    def to_dict(self) -> dict[str, Any]:
        path = self.path

        interval_s = self.interval_s

        timeout_s = self.timeout_s

        retries = self.retries

        field_dict: dict[str, Any] = {}
        field_dict.update(self.additional_properties)
        field_dict.update(
            {
                "path": path,
            }
        )
        if interval_s is not UNSET:
            field_dict["interval_s"] = interval_s
        if timeout_s is not UNSET:
            field_dict["timeout_s"] = timeout_s
        if retries is not UNSET:
            field_dict["retries"] = retries

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        path = d.pop("path")

        interval_s = d.pop("interval_s", UNSET)

        timeout_s = d.pop("timeout_s", UNSET)

        retries = d.pop("retries", UNSET)

        deployment_healthcheck = cls(
            path=path,
            interval_s=interval_s,
            timeout_s=timeout_s,
            retries=retries,
        )

        deployment_healthcheck.additional_properties = d
        return deployment_healthcheck

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
