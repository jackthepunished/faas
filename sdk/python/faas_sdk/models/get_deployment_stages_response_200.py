from __future__ import annotations

import datetime
from collections.abc import Mapping
from typing import TYPE_CHECKING, Any, TypeVar, cast

from attrs import define as _attrs_define
from attrs import field as _attrs_field

from ..models.get_deployment_stages_response_200_current import (
    GetDeploymentStagesResponse200Current,
    check_get_deployment_stages_response_200_current,
)
from ..types import UNSET, Unset

if TYPE_CHECKING:
    from ..models.get_deployment_stages_response_200_history_item import GetDeploymentStagesResponse200HistoryItem


T = TypeVar("T", bound="GetDeploymentStagesResponse200")


@_attrs_define
class GetDeploymentStagesResponse200:
    """The raw `deployments.stage_state` jsonb. Closed-vocabulary enforcement lives at the database layer
    (`deployments_stage_state_current_check`).

    """

    current: GetDeploymentStagesResponse200Current | Unset = UNSET
    current_started_at: datetime.datetime | None | Unset = UNSET
    history: list[GetDeploymentStagesResponse200HistoryItem] | Unset = UNSET
    additional_properties: dict[str, Any] = _attrs_field(init=False, factory=dict)

    def to_dict(self) -> dict[str, Any]:
        current: str | Unset = UNSET
        if not isinstance(self.current, Unset):
            current = self.current

        current_started_at: None | str | Unset
        if isinstance(self.current_started_at, Unset):
            current_started_at = UNSET
        elif isinstance(self.current_started_at, datetime.datetime):
            current_started_at = self.current_started_at.isoformat()
        else:
            current_started_at = self.current_started_at

        history: list[dict[str, Any]] | Unset = UNSET
        if not isinstance(self.history, Unset):
            history = []
            for history_item_data in self.history:
                history_item = history_item_data.to_dict()
                history.append(history_item)

        field_dict: dict[str, Any] = {}
        field_dict.update(self.additional_properties)
        field_dict.update({})
        if current is not UNSET:
            field_dict["current"] = current
        if current_started_at is not UNSET:
            field_dict["current_started_at"] = current_started_at
        if history is not UNSET:
            field_dict["history"] = history

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        from ..models.get_deployment_stages_response_200_history_item import GetDeploymentStagesResponse200HistoryItem

        d = dict(src_dict)
        _current = d.pop("current", UNSET)
        current: GetDeploymentStagesResponse200Current | Unset
        if isinstance(_current, Unset):
            current = UNSET
        else:
            current = check_get_deployment_stages_response_200_current(_current)

        def _parse_current_started_at(data: object) -> datetime.datetime | None | Unset:
            if data is None:
                return data
            if isinstance(data, Unset):
                return data
            try:
                if not isinstance(data, str):
                    raise TypeError()
                current_started_at_type_0 = datetime.datetime.fromisoformat(data)

                return current_started_at_type_0
            except (TypeError, ValueError, AttributeError, KeyError):
                pass
            return cast(datetime.datetime | None | Unset, data)

        current_started_at = _parse_current_started_at(d.pop("current_started_at", UNSET))

        _history = d.pop("history", UNSET)
        history: list[GetDeploymentStagesResponse200HistoryItem] | Unset = UNSET
        if _history is not UNSET:
            history = []
            for history_item_data in _history:
                history_item = GetDeploymentStagesResponse200HistoryItem.from_dict(history_item_data)

                history.append(history_item)

        get_deployment_stages_response_200 = cls(
            current=current,
            current_started_at=current_started_at,
            history=history,
        )

        get_deployment_stages_response_200.additional_properties = d
        return get_deployment_stages_response_200

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
