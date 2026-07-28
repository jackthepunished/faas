from __future__ import annotations

from collections.abc import Mapping
from typing import TYPE_CHECKING, Any, TypeVar

from attrs import define as _attrs_define
from attrs import field as _attrs_field

if TYPE_CHECKING:
    from ..models.app_env_response import AppEnvResponse


T = TypeVar("T", bound="AppEnvListResponse")


@_attrs_define
class AppEnvListResponse:
    """List of env var envelopes (no plaintext values)."""

    env: list[AppEnvResponse]
    quota_max: int
    count: int
    additional_properties: dict[str, Any] = _attrs_field(init=False, factory=dict)

    def to_dict(self) -> dict[str, Any]:
        env = []
        for env_item_data in self.env:
            env_item = env_item_data.to_dict()
            env.append(env_item)

        quota_max = self.quota_max

        count = self.count

        field_dict: dict[str, Any] = {}
        field_dict.update(self.additional_properties)
        field_dict.update(
            {
                "env": env,
                "quota_max": quota_max,
                "count": count,
            }
        )

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        from ..models.app_env_response import AppEnvResponse

        d = dict(src_dict)
        env = []
        _env = d.pop("env")
        for env_item_data in _env:
            env_item = AppEnvResponse.from_dict(env_item_data)

            env.append(env_item)

        quota_max = d.pop("quota_max")

        count = d.pop("count")

        app_env_list_response = cls(
            env=env,
            quota_max=quota_max,
            count=count,
        )

        app_env_list_response.additional_properties = d
        return app_env_list_response

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
