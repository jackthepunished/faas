from __future__ import annotations

from collections.abc import Mapping
from typing import Any, TypeVar, cast

from attrs import define as _attrs_define
from attrs import field as _attrs_field

from ..models.source_tarball_deploy_request_tag_type_1 import (
    SourceTarballDeployRequestTagType1,
    check_source_tarball_deploy_request_tag_type_1,
)
from ..models.source_tarball_deploy_request_tag_type_2_type_1 import (
    SourceTarballDeployRequestTagType2Type1,
    check_source_tarball_deploy_request_tag_type_2_type_1,
)
from ..models.source_tarball_deploy_request_tag_type_3_type_1 import (
    SourceTarballDeployRequestTagType3Type1,
    check_source_tarball_deploy_request_tag_type_3_type_1,
)
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
    reason: None | str | Unset = UNSET
    tag: (
        None
        | SourceTarballDeployRequestTagType1
        | SourceTarballDeployRequestTagType2Type1
        | SourceTarballDeployRequestTagType3Type1
        | Unset
    ) = UNSET
    deployed_by: None | str | Unset = UNSET
    pr_number: int | None | Unset = UNSET
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

        reason: None | str | Unset
        if isinstance(self.reason, Unset):
            reason = UNSET
        else:
            reason = self.reason

        tag: None | str | Unset
        if isinstance(self.tag, Unset):
            tag = UNSET
        elif isinstance(self.tag, str):
            tag = self.tag
        elif isinstance(self.tag, str):
            tag = self.tag
        elif isinstance(self.tag, str):
            tag = self.tag
        else:
            tag = self.tag

        deployed_by: None | str | Unset
        if isinstance(self.deployed_by, Unset):
            deployed_by = UNSET
        else:
            deployed_by = self.deployed_by

        pr_number: int | None | Unset
        if isinstance(self.pr_number, Unset):
            pr_number = UNSET
        else:
            pr_number = self.pr_number

        field_dict: dict[str, Any] = {}
        field_dict.update(self.additional_properties)
        field_dict.update({})
        if repo is not UNSET:
            field_dict["repo"] = repo
        if ref is not UNSET:
            field_dict["ref"] = ref
        if reason is not UNSET:
            field_dict["reason"] = reason
        if tag is not UNSET:
            field_dict["tag"] = tag
        if deployed_by is not UNSET:
            field_dict["deployed_by"] = deployed_by
        if pr_number is not UNSET:
            field_dict["pr_number"] = pr_number

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

        def _parse_reason(data: object) -> None | str | Unset:
            if data is None:
                return data
            if isinstance(data, Unset):
                return data
            return cast(None | str | Unset, data)

        reason = _parse_reason(d.pop("reason", UNSET))

        def _parse_tag(
            data: object,
        ) -> (
            None
            | SourceTarballDeployRequestTagType1
            | SourceTarballDeployRequestTagType2Type1
            | SourceTarballDeployRequestTagType3Type1
            | Unset
        ):
            if data is None:
                return data
            if isinstance(data, Unset):
                return data
            try:
                if not isinstance(data, str):
                    raise TypeError()
                tag_type_1 = check_source_tarball_deploy_request_tag_type_1(data)

                return tag_type_1
            except (TypeError, ValueError, AttributeError, KeyError):
                pass
            try:
                if not isinstance(data, str):
                    raise TypeError()
                tag_type_2_type_1 = check_source_tarball_deploy_request_tag_type_2_type_1(data)

                return tag_type_2_type_1
            except (TypeError, ValueError, AttributeError, KeyError):
                pass
            try:
                if not isinstance(data, str):
                    raise TypeError()
                tag_type_3_type_1 = check_source_tarball_deploy_request_tag_type_3_type_1(data)

                return tag_type_3_type_1
            except (TypeError, ValueError, AttributeError, KeyError):
                pass
            return cast(
                None
                | SourceTarballDeployRequestTagType1
                | SourceTarballDeployRequestTagType2Type1
                | SourceTarballDeployRequestTagType3Type1
                | Unset,
                data,
            )

        tag = _parse_tag(d.pop("tag", UNSET))

        def _parse_deployed_by(data: object) -> None | str | Unset:
            if data is None:
                return data
            if isinstance(data, Unset):
                return data
            return cast(None | str | Unset, data)

        deployed_by = _parse_deployed_by(d.pop("deployed_by", UNSET))

        def _parse_pr_number(data: object) -> int | None | Unset:
            if data is None:
                return data
            if isinstance(data, Unset):
                return data
            return cast(int | None | Unset, data)

        pr_number = _parse_pr_number(d.pop("pr_number", UNSET))

        source_tarball_deploy_request = cls(
            repo=repo,
            ref=ref,
            reason=reason,
            tag=tag,
            deployed_by=deployed_by,
            pr_number=pr_number,
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
