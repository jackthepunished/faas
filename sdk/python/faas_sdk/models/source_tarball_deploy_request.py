from __future__ import annotations

from collections.abc import Mapping
from typing import Any, TypeVar, cast

from attrs import define as _attrs_define
from attrs import field as _attrs_field

from ..types import UNSET, Unset

T = TypeVar("T", bound="SourceTarballDeployRequest")


@_attrs_define
class SourceTarballDeployRequest:
    """Body for the informational `sidecar` form field on POST /v1/apps/{slug}/deployments/source-tarball (issue #961 /
    Mega-A PR-1, ADR-115). The CLI is the trust root for this deploy path; apid does NOT consult `github_installations`
    and does NOT attempt a server-side git fetch. The sidecar fields are recorded on the build row for provenance only —
    the build pipeline does NOT use them to fetch upstream.

    """

    repo: None | str | Unset = UNSET
    """`owner/repo` from the customer's git remote, parsed by `cmd/gregale/git_local.go::parseGitRemoteURL`. nil
    when the sidecar is omitted entirely."""
    ref: None | str | Unset = UNSET
    """40-char lowercase SHA from `git rev-parse HEAD`. Informational only; the build pipeline does NOT pin to this
    SHA."""
    additional_properties: dict[str, Any] = _attrs_field(init=False, factory=dict)

    def to_dict(self) -> dict[str, Any]:
        repo: None | str | Unset
        if isinstance(self.repo, Unset):
            repo = UNSET
        else:
            repo = self.repo

        ref: None | str | Unset
        if isinstance(self.ref, Unset):
            ref = UNSET
        else:
            ref = self.ref

        field_dict: dict[str, Any] = {}
        field_dict.update(self.additional_properties)
        field_dict.update({})
        if repo is not UNSET:
            field_dict["repo"] = repo
        if ref is not UNSET:
            field_dict["ref"] = ref

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)

        def _parse_repo(data: object) -> None | str | Unset:
            if data is None:
                return data
            if isinstance(data, Unset):
                return data
            return cast(None | str | Unset, data)

        repo = _parse_repo(d.pop("repo", UNSET))

        def _parse_ref(data: object) -> None | str | Unset:
            if data is None:
                return data
            if isinstance(data, Unset):
                return data
            return cast(None | str | Unset, data)

        ref = _parse_ref(d.pop("ref", UNSET))

        source_tarball_deploy_request = cls(
            repo=repo,
            ref=ref,
        )

        source_tarball_deploy_request.additional_properties = d
        return source_tarball_deploy_request

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
