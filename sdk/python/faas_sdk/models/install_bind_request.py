from __future__ import annotations

from collections.abc import Mapping
from typing import Any, TypeVar

from attrs import define as _attrs_define
from attrs import field as _attrs_field

from ..types import UNSET, Unset

T = TypeVar("T", bound="InstallBindRequest")


@_attrs_define
class InstallBindRequest:
    """Body for both `POST /v1/install/repos/list` and
    `POST /v1/apps/{slug}/install/bind`. Carries the
    (installation_id, repo_full_name, production_branch) tuple
    the dashboard's bind picker commits. `production_branch` is
    optional — when omitted, githubd uses the install's
    `default_branch` from `/installations/{id}`.

    """

    installation_id: int
    repo_full_name: str
    production_branch: str | Unset = UNSET
    additional_properties: dict[str, Any] = _attrs_field(init=False, factory=dict)

    def to_dict(self) -> dict[str, Any]:
        installation_id = self.installation_id

        repo_full_name = self.repo_full_name

        production_branch = self.production_branch

        field_dict: dict[str, Any] = {}
        field_dict.update(self.additional_properties)
        field_dict.update(
            {
                "installation_id": installation_id,
                "repo_full_name": repo_full_name,
            }
        )
        if production_branch is not UNSET:
            field_dict["production_branch"] = production_branch

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        installation_id = d.pop("installation_id")

        repo_full_name = d.pop("repo_full_name")

        production_branch = d.pop("production_branch", UNSET)

        install_bind_request = cls(
            installation_id=installation_id,
            repo_full_name=repo_full_name,
            production_branch=production_branch,
        )

        install_bind_request.additional_properties = d
        return install_bind_request

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
