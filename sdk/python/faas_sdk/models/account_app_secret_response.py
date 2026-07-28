from __future__ import annotations

import datetime
from collections.abc import Mapping
from typing import Any, TypeVar

from attrs import define as _attrs_define
from attrs import field as _attrs_field

T = TypeVar("T", bound="AccountAppSecretResponse")


@_attrs_define
class AccountAppSecretResponse:
    """One row in `GET /v1/secrets` — a sealed envelope on a specific
    app (issue #393). Plaintext NEVER appears here: only the
    age-sealed envelope (base64). `app_id` and `app_slug` let the
    dashboard render "foo-app / DATABASE_URL" without a parallel
    `/v1/apps` round-trip.

    """

    app_id: str
    app_slug: str
    key: str
    ciphertext: str
    """base64 age-sealed envelope. Plaintext NEVER appears on this wire."""
    created_at: datetime.datetime
    updated_at: datetime.datetime
    additional_properties: dict[str, Any] = _attrs_field(init=False, factory=dict)

    def to_dict(self) -> dict[str, Any]:
        app_id = self.app_id

        app_slug = self.app_slug

        key = self.key

        ciphertext = self.ciphertext

        created_at = self.created_at.isoformat()

        updated_at = self.updated_at.isoformat()

        field_dict: dict[str, Any] = {}
        field_dict.update(self.additional_properties)
        field_dict.update(
            {
                "app_id": app_id,
                "app_slug": app_slug,
                "key": key,
                "ciphertext": ciphertext,
                "created_at": created_at,
                "updated_at": updated_at,
            }
        )

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        app_id = d.pop("app_id")

        app_slug = d.pop("app_slug")

        key = d.pop("key")

        ciphertext = d.pop("ciphertext")

        created_at = datetime.datetime.fromisoformat(d.pop("created_at"))

        updated_at = datetime.datetime.fromisoformat(d.pop("updated_at"))

        account_app_secret_response = cls(
            app_id=app_id,
            app_slug=app_slug,
            key=key,
            ciphertext=ciphertext,
            created_at=created_at,
            updated_at=updated_at,
        )

        account_app_secret_response.additional_properties = d
        return account_app_secret_response

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
