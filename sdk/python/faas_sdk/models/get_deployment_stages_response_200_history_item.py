from __future__ import annotations

import datetime
from collections.abc import Mapping
from typing import Any, TypeVar, cast

from attrs import define as _attrs_define
from attrs import field as _attrs_field

from ..models.get_deployment_stages_response_200_history_item_name import (
    GetDeploymentStagesResponse200HistoryItemName,
    check_get_deployment_stages_response_200_history_item_name,
)
from ..models.get_deployment_stages_response_200_history_item_status import (
    GetDeploymentStagesResponse200HistoryItemStatus,
    check_get_deployment_stages_response_200_history_item_status,
)
from ..types import UNSET, Unset

T = TypeVar("T", bound="GetDeploymentStagesResponse200HistoryItem")


@_attrs_define
class GetDeploymentStagesResponse200HistoryItem:
    name: GetDeploymentStagesResponse200HistoryItemName | Unset = UNSET
    started_at: datetime.datetime | None | Unset = UNSET
    ended_at: datetime.datetime | None | Unset = UNSET
    duration_ms: int | Unset = UNSET
    status: GetDeploymentStagesResponse200HistoryItemStatus | Unset = UNSET
    reason: str | Unset = UNSET
    additional_properties: dict[str, Any] = _attrs_field(init=False, factory=dict)

    def to_dict(self) -> dict[str, Any]:
        name: str | Unset = UNSET
        if not isinstance(self.name, Unset):
            name = self.name

        started_at: None | str | Unset
        if isinstance(self.started_at, Unset):
            started_at = UNSET
        elif isinstance(self.started_at, datetime.datetime):
            started_at = self.started_at.isoformat()
        else:
            started_at = self.started_at

        ended_at: None | str | Unset
        if isinstance(self.ended_at, Unset):
            ended_at = UNSET
        elif isinstance(self.ended_at, datetime.datetime):
            ended_at = self.ended_at.isoformat()
        else:
            ended_at = self.ended_at

        duration_ms = self.duration_ms

        status: str | Unset = UNSET
        if not isinstance(self.status, Unset):
            status = self.status

        reason = self.reason

        field_dict: dict[str, Any] = {}
        field_dict.update(self.additional_properties)
        field_dict.update({})
        if name is not UNSET:
            field_dict["name"] = name
        if started_at is not UNSET:
            field_dict["started_at"] = started_at
        if ended_at is not UNSET:
            field_dict["ended_at"] = ended_at
        if duration_ms is not UNSET:
            field_dict["duration_ms"] = duration_ms
        if status is not UNSET:
            field_dict["status"] = status
        if reason is not UNSET:
            field_dict["reason"] = reason

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        _name = d.pop("name", UNSET)
        name: GetDeploymentStagesResponse200HistoryItemName | Unset
        if isinstance(_name, Unset):
            name = UNSET
        else:
            name = check_get_deployment_stages_response_200_history_item_name(_name)

        def _parse_started_at(data: object) -> datetime.datetime | None | Unset:
            if data is None:
                return data
            if isinstance(data, Unset):
                return data
            try:
                if not isinstance(data, str):
                    raise TypeError()
                started_at_type_0 = datetime.datetime.fromisoformat(data)

                return started_at_type_0
            except (TypeError, ValueError, AttributeError, KeyError):
                pass
            return cast(datetime.datetime | None | Unset, data)

        started_at = _parse_started_at(d.pop("started_at", UNSET))

        def _parse_ended_at(data: object) -> datetime.datetime | None | Unset:
            if data is None:
                return data
            if isinstance(data, Unset):
                return data
            try:
                if not isinstance(data, str):
                    raise TypeError()
                ended_at_type_0 = datetime.datetime.fromisoformat(data)

                return ended_at_type_0
            except (TypeError, ValueError, AttributeError, KeyError):
                pass
            return cast(datetime.datetime | None | Unset, data)

        ended_at = _parse_ended_at(d.pop("ended_at", UNSET))

        duration_ms = d.pop("duration_ms", UNSET)

        _status = d.pop("status", UNSET)
        status: GetDeploymentStagesResponse200HistoryItemStatus | Unset
        if isinstance(_status, Unset):
            status = UNSET
        else:
            status = check_get_deployment_stages_response_200_history_item_status(_status)

        reason = d.pop("reason", UNSET)

        get_deployment_stages_response_200_history_item = cls(
            name=name,
            started_at=started_at,
            ended_at=ended_at,
            duration_ms=duration_ms,
            status=status,
            reason=reason,
        )

        get_deployment_stages_response_200_history_item.additional_properties = d
        return get_deployment_stages_response_200_history_item

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
