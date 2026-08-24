from __future__ import annotations

from collections.abc import Mapping
from typing import Any, TypeVar

from attrs import define as _attrs_define
from attrs import field as _attrs_field

from ..models.source_ref_deploy_request_format import (
    SourceRefDeployRequestFormat,
    check_source_ref_deploy_request_format,
)
from ..models.source_ref_deploy_request_tag import SourceRefDeployRequestTag, check_source_ref_deploy_request_tag
from ..types import UNSET, Unset

T = TypeVar("T", bound="SourceRefDeployRequest")


@_attrs_define
class SourceRefDeployRequest:
    """JSON body for POST /v1/apps/{slug}/deployments/source-ref
    (DEPLOY-PROV-4 / ADR-092, issue #739). The headless CI deploy
    path: `repo` resolves to an install-token-bound fetch, `ref` is
    the customer's chosen input (branch / tag / SHA — server
    resolves to a 40-char SHA before stamping the deployment row).

    """

    repo: str
    """GitHub owner/name slug, e.g. `onebox-faas/hello`."""
    ref: str
    """Commit ref — 40-char SHA, branch, or tag. api.github.com
    /repos/<repo>/commits/<ref> resolves branch / tag inputs
    to a pinned SHA before the tarball fetch starts. The wire
    shape pins to the resolved 40-char SHA (server override;
    caller's `ref` is preserved on the `deploy.source_ref`
    audit row for traceability).
    """
    format_: SourceRefDeployRequestFormat | Unset = "tarball"
    """Forward-compat field. PR-A only supports `tarball`."""
    reason: str | Unset = UNSET
    """Free-form operator note (≤280 chars). Example: 'Emergency rollback after payment provider incident'."""
    tag: SourceRefDeployRequestTag | Unset = UNSET
    """Closed-set annotation tag for grouping/filtering."""
    deployed_by: str | Unset = UNSET
    """Human-readable actor label. CLI auto-captures from `git config user.name`; githubd stamps pusher.name; the
    GitHub Action defaults to ${{ github.actor }}."""
    pr_number: int | Unset = UNSET
    """Pull-request number when the wire offers it (githubd pull_request.number; Action ${{
    github.event.pull_request.number }}). NULL for push-to-main with no inferred PR."""
    additional_properties: dict[str, Any] = _attrs_field(init=False, factory=dict)

    def to_dict(self) -> dict[str, Any]:
        repo = self.repo

        ref = self.ref

        format_: str | Unset = UNSET
        if not isinstance(self.format_, Unset):
            format_ = self.format_

        reason = self.reason

        tag: str | Unset = UNSET
        if not isinstance(self.tag, Unset):
            tag = self.tag

        deployed_by = self.deployed_by

        pr_number = self.pr_number

        field_dict: dict[str, Any] = {}
        field_dict.update(self.additional_properties)
        field_dict.update(
            {
                "repo": repo,
                "ref": ref,
            }
        )
        if format_ is not UNSET:
            field_dict["format"] = format_
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
        repo = d.pop("repo")

        ref = d.pop("ref")

        _format_ = d.pop("format", UNSET)
        format_: SourceRefDeployRequestFormat | Unset
        if isinstance(_format_, Unset):
            format_ = UNSET
        else:
            format_ = check_source_ref_deploy_request_format(_format_)

        reason = d.pop("reason", UNSET)

        _tag = d.pop("tag", UNSET)
        tag: SourceRefDeployRequestTag | Unset
        if isinstance(_tag, Unset):
            tag = UNSET
        else:
            tag = check_source_ref_deploy_request_tag(_tag)

        deployed_by = d.pop("deployed_by", UNSET)

        pr_number = d.pop("pr_number", UNSET)

        source_ref_deploy_request = cls(
            repo=repo,
            ref=ref,
            format_=format_,
            reason=reason,
            tag=tag,
            deployed_by=deployed_by,
            pr_number=pr_number,
        )

        source_ref_deploy_request.additional_properties = d
        return source_ref_deploy_request

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
