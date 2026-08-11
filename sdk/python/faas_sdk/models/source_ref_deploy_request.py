from __future__ import annotations

from collections.abc import Mapping
from typing import Any, TypeVar

from attrs import define as _attrs_define
from attrs import field as _attrs_field

from ..models.source_ref_deploy_request_format import (
    SourceRefDeployRequestFormat,
    check_source_ref_deploy_request_format,
)
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
    additional_properties: dict[str, Any] = _attrs_field(init=False, factory=dict)

    def to_dict(self) -> dict[str, Any]:
        repo = self.repo

        ref = self.ref

        format_: str | Unset = UNSET
        if not isinstance(self.format_, Unset):
            format_ = self.format_

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

        source_ref_deploy_request = cls(
            repo=repo,
            ref=ref,
            format_=format_,
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
