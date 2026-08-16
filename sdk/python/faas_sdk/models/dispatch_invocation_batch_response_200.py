from __future__ import annotations

from collections.abc import Mapping
from typing import Any, TypeVar, cast

from attrs import define as _attrs_define
from attrs import field as _attrs_field

from ..types import UNSET, Unset

T = TypeVar("T", bound="DispatchInvocationBatchResponse200")


@_attrs_define
class DispatchInvocationBatchResponse200:
    succeeded: list[str] | Unset = UNSET
    retry: list[str] | Unset = UNSET
    dead_letter: list[str] | Unset = UNSET
    additional_properties: dict[str, Any] = _attrs_field(init=False, factory=dict)

    def to_dict(self) -> dict[str, Any]:
        succeeded: list[str] | Unset = UNSET
        if not isinstance(self.succeeded, Unset):
            succeeded = self.succeeded

        retry: list[str] | Unset = UNSET
        if not isinstance(self.retry, Unset):
            retry = self.retry

        dead_letter: list[str] | Unset = UNSET
        if not isinstance(self.dead_letter, Unset):
            dead_letter = self.dead_letter

        field_dict: dict[str, Any] = {}
        field_dict.update(self.additional_properties)
        field_dict.update({})
        if succeeded is not UNSET:
            field_dict["succeeded"] = succeeded
        if retry is not UNSET:
            field_dict["retry"] = retry
        if dead_letter is not UNSET:
            field_dict["dead_letter"] = dead_letter

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        succeeded = cast(list[str], d.pop("succeeded", UNSET))

        retry = cast(list[str], d.pop("retry", UNSET))

        dead_letter = cast(list[str], d.pop("dead_letter", UNSET))

        dispatch_invocation_batch_response_200 = cls(
            succeeded=succeeded,
            retry=retry,
            dead_letter=dead_letter,
        )

        dispatch_invocation_batch_response_200.additional_properties = d
        return dispatch_invocation_batch_response_200

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
