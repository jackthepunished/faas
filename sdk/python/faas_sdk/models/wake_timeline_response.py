from __future__ import annotations

from collections.abc import Mapping
from typing import TYPE_CHECKING, Any, TypeVar
from uuid import UUID

from attrs import define as _attrs_define
from attrs import field as _attrs_field

from ..types import UNSET, Unset

if TYPE_CHECKING:
    from ..models.wake_timeline_event import WakeTimelineEvent


T = TypeVar("T", bound="WakeTimelineResponse")


@_attrs_define
class WakeTimelineResponse:
    """Envelope for `GET /v1/apps/{slug}/wakes/{wake_id}/timeline`."""

    wake_id: str
    """Echo of the path-segment wake_id."""
    app_id: UUID
    """Resolved app id (the slug's owning app)."""
    events: list[WakeTimelineEvent]
    limit: int
    """Effective limit applied (always 1..1000)."""
    next_cursor: str | Unset = UNSET
    """Opaque RFC 3339 cursor for the next page. Empty when this is the last page."""
    additional_properties: dict[str, Any] = _attrs_field(init=False, factory=dict)

    def to_dict(self) -> dict[str, Any]:
        wake_id = self.wake_id

        app_id = str(self.app_id)

        events = []
        for events_item_data in self.events:
            events_item = events_item_data.to_dict()
            events.append(events_item)

        limit = self.limit

        next_cursor = self.next_cursor

        field_dict: dict[str, Any] = {}
        field_dict.update(self.additional_properties)
        field_dict.update(
            {
                "wake_id": wake_id,
                "app_id": app_id,
                "events": events,
                "limit": limit,
            }
        )
        if next_cursor is not UNSET:
            field_dict["next_cursor"] = next_cursor

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        from ..models.wake_timeline_event import WakeTimelineEvent

        d = dict(src_dict)
        wake_id = d.pop("wake_id")

        app_id = UUID(d.pop("app_id"))

        events = []
        _events = d.pop("events")
        for events_item_data in _events:
            events_item = WakeTimelineEvent.from_dict(events_item_data)

            events.append(events_item)

        limit = d.pop("limit")

        next_cursor = d.pop("next_cursor", UNSET)

        wake_timeline_response = cls(
            wake_id=wake_id,
            app_id=app_id,
            events=events,
            limit=limit,
            next_cursor=next_cursor,
        )

        wake_timeline_response.additional_properties = d
        return wake_timeline_response

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
