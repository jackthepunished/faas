from __future__ import annotations

from collections.abc import Mapping
from typing import Any, TypeVar

from attrs import define as _attrs_define
from attrs import field as _attrs_field

T = TypeVar("T", bound="InstallBindResponse")


@_attrs_define
class InstallBindResponse:
    """Successful bind. `binding_id` is the deterministic `bind-<appID>-<repo>` form used in audit log entries."""

    binding_id: str
    repo_full_name: str
    production_branch: str
    additional_properties: dict[str, Any] = _attrs_field(init=False, factory=dict)

    def to_dict(self) -> dict[str, Any]:
        binding_id = self.binding_id

        repo_full_name = self.repo_full_name

        production_branch = self.production_branch

        field_dict: dict[str, Any] = {}
        field_dict.update(self.additional_properties)
        field_dict.update(
            {
                "binding_id": binding_id,
                "repo_full_name": repo_full_name,
                "production_branch": production_branch,
            }
        )

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        binding_id = d.pop("binding_id")

        repo_full_name = d.pop("repo_full_name")

        production_branch = d.pop("production_branch")

        install_bind_response = cls(
            binding_id=binding_id,
            repo_full_name=repo_full_name,
            production_branch=production_branch,
        )

        install_bind_response.additional_properties = d
        return install_bind_response

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
