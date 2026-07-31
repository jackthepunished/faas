from __future__ import annotations

from collections.abc import Mapping
from io import BytesIO
from typing import Any, TypeVar

from attrs import define as _attrs_define
from attrs import field as _attrs_field

from .. import types
from ..types import UNSET, File, Unset

T = TypeVar("T", bound="ProjectScanRequest")


@_attrs_define
class ProjectScanRequest:
    """Multipart body for POST /v1/projects/scan (dry-run)."""

    source: File
    """tar.gz of the repo root."""
    project_slug: str | Unset = UNSET
    """kebab slug; default = repo dir basename"""
    production_branch: str | Unset = UNSET
    install_id: int | Unset = UNSET
    """GitHub install id (with --repo); 0 for unbound repos"""
    only: str | Unset = UNSET
    """CSV of workload names to include (others skipped)"""
    additional_properties: dict[str, Any] = _attrs_field(init=False, factory=dict)

    def to_dict(self) -> dict[str, Any]:
        source = self.source.to_tuple()

        project_slug = self.project_slug

        production_branch = self.production_branch

        install_id = self.install_id

        only = self.only

        field_dict: dict[str, Any] = {}
        field_dict.update(self.additional_properties)
        field_dict.update(
            {
                "source": source,
            }
        )
        if project_slug is not UNSET:
            field_dict["project_slug"] = project_slug
        if production_branch is not UNSET:
            field_dict["production_branch"] = production_branch
        if install_id is not UNSET:
            field_dict["install_id"] = install_id
        if only is not UNSET:
            field_dict["only"] = only

        return field_dict

    def to_multipart(self) -> types.RequestFiles:
        files: types.RequestFiles = []

        files.append(("source", self.source.to_tuple()))

        if not isinstance(self.project_slug, Unset):
            files.append(("project_slug", (None, str(self.project_slug).encode(), "text/plain")))

        if not isinstance(self.production_branch, Unset):
            files.append(("production_branch", (None, str(self.production_branch).encode(), "text/plain")))

        if not isinstance(self.install_id, Unset):
            files.append(("install_id", (None, str(self.install_id).encode(), "text/plain")))

        if not isinstance(self.only, Unset):
            files.append(("only", (None, str(self.only).encode(), "text/plain")))

        for prop_name, prop in self.additional_properties.items():
            files.append((prop_name, (None, str(prop).encode(), "text/plain")))

        return files

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        source = File(payload=BytesIO(d.pop("source")))

        project_slug = d.pop("project_slug", UNSET)

        production_branch = d.pop("production_branch", UNSET)

        install_id = d.pop("install_id", UNSET)

        only = d.pop("only", UNSET)

        project_scan_request = cls(
            source=source,
            project_slug=project_slug,
            production_branch=production_branch,
            install_id=install_id,
            only=only,
        )

        project_scan_request.additional_properties = d
        return project_scan_request

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
