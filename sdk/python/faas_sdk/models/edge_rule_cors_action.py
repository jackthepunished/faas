from __future__ import annotations

from collections.abc import Mapping
from typing import Any, TypeVar, cast

from attrs import define as _attrs_define
from attrs import field as _attrs_field

from ..types import UNSET, Unset

T = TypeVar("T", bound="EdgeRuleCORSAction")


@_attrs_define
class EdgeRuleCORSAction:
    """Stamps CORS headers + handles preflight in-process."""

    allow_origins: list[str]
    allow_methods: list[str]
    allow_headers: list[str] | Unset = UNSET
    expose_headers: list[str] | Unset = UNSET
    allow_credentials: bool | Unset = False
    max_age_seconds: int | Unset = UNSET
    additional_properties: dict[str, Any] = _attrs_field(init=False, factory=dict)

    def to_dict(self) -> dict[str, Any]:
        allow_origins = self.allow_origins

        allow_methods = self.allow_methods

        allow_headers: list[str] | Unset = UNSET
        if not isinstance(self.allow_headers, Unset):
            allow_headers = self.allow_headers

        expose_headers: list[str] | Unset = UNSET
        if not isinstance(self.expose_headers, Unset):
            expose_headers = self.expose_headers

        allow_credentials = self.allow_credentials

        max_age_seconds = self.max_age_seconds

        field_dict: dict[str, Any] = {}
        field_dict.update(self.additional_properties)
        field_dict.update(
            {
                "allow_origins": allow_origins,
                "allow_methods": allow_methods,
            }
        )
        if allow_headers is not UNSET:
            field_dict["allow_headers"] = allow_headers
        if expose_headers is not UNSET:
            field_dict["expose_headers"] = expose_headers
        if allow_credentials is not UNSET:
            field_dict["allow_credentials"] = allow_credentials
        if max_age_seconds is not UNSET:
            field_dict["max_age_seconds"] = max_age_seconds

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        allow_origins = cast(list[str], d.pop("allow_origins"))

        allow_methods = cast(list[str], d.pop("allow_methods"))

        allow_headers = cast(list[str], d.pop("allow_headers", UNSET))

        expose_headers = cast(list[str], d.pop("expose_headers", UNSET))

        allow_credentials = d.pop("allow_credentials", UNSET)

        max_age_seconds = d.pop("max_age_seconds", UNSET)

        edge_rule_cors_action = cls(
            allow_origins=allow_origins,
            allow_methods=allow_methods,
            allow_headers=allow_headers,
            expose_headers=expose_headers,
            allow_credentials=allow_credentials,
            max_age_seconds=max_age_seconds,
        )

        edge_rule_cors_action.additional_properties = d
        return edge_rule_cors_action

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
