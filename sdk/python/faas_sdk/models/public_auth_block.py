from __future__ import annotations

from collections.abc import Mapping
from typing import Any, TypeVar

from attrs import define as _attrs_define

from ..models.public_auth_block_mode import PublicAuthBlockMode, check_public_auth_block_mode
from ..types import UNSET, Unset

T = TypeVar("T", bound="PublicAuthBlock")


@_attrs_define
class PublicAuthBlock:
    """Per-app public-URL auth write shape (issue #477 / ADR-077). Sent on PATCH /v1/apps/{slug}; apid seals the basic_user
    + basic_pass into a single APP_BASIC_AUTH secretbox blob before persistence. The plaintext is never echoed on read
    (see PublicAuthStatus).

    """

    mode: PublicAuthBlockMode
    """Auth mode (closed set). 'open' is the pre-#477 default (every request passes). 'bearer' requires
    Authorization: Bearer (Hobby+; 402 on Free). 'basic' requires HTTP Basic auth with sealed credentials (Pro+; 402
    on Free/Hobby). Unknown values → 422 invalid_public_auth_mode."""
    basic_user: str | Unset = UNSET
    """Basic-auth username (RFC 7617 §2). Plaintext at PATCH time; sealed into apps.public_auth_basic under the
    APP_BASIC_AUTH secretbox namespace. Required when mode='basic'; ignored otherwise. Range [1, 128] bytes after
    TrimSpace."""
    basic_pass: str | Unset = UNSET
    """Basic-auth password (RFC 7617 §2). Plaintext at PATCH time; sealed alongside basic_user under the
    APP_BASIC_AUTH secretbox namespace. Required when mode='basic'; ignored otherwise. Range [1, 256] bytes."""

    def to_dict(self) -> dict[str, Any]:
        mode: str = self.mode

        basic_user = self.basic_user

        basic_pass = self.basic_pass

        field_dict: dict[str, Any] = {}

        field_dict.update(
            {
                "mode": mode,
            }
        )
        if basic_user is not UNSET:
            field_dict["basic_user"] = basic_user
        if basic_pass is not UNSET:
            field_dict["basic_pass"] = basic_pass

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        mode = check_public_auth_block_mode(d.pop("mode"))

        basic_user = d.pop("basic_user", UNSET)

        basic_pass = d.pop("basic_pass", UNSET)

        public_auth_block = cls(
            mode=mode,
            basic_user=basic_user,
            basic_pass=basic_pass,
        )

        return public_auth_block
