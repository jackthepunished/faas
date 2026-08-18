from __future__ import annotations

import datetime
from collections.abc import Mapping
from typing import Any, TypeVar, cast

from attrs import define as _attrs_define
from attrs import field as _attrs_field

from ..types import UNSET, Unset

T = TypeVar("T", bound="AppErrorRequestItem")


@_attrs_define
class AppErrorRequestItem:
    """One row of the drill-down page (ADR-096 / PR-B)."""

    request_id: str
    received_at: datetime.datetime
    route: str
    http_status: int
    error_class: str
    sample_message: str
    deployment_id: None | str | Unset = UNSET
    """Nullable — the FK is ON DELETE SET NULL so an evicted deployment leaves the drill-down row intact."""
    additional_properties: dict[str, Any] = _attrs_field(init=False, factory=dict)

    def to_dict(self) -> dict[str, Any]:
        request_id = self.request_id

        received_at = self.received_at.isoformat()

        route = self.route

        http_status = self.http_status

        error_class = self.error_class

        sample_message = self.sample_message

        deployment_id: None | str | Unset
        if isinstance(self.deployment_id, Unset):
            deployment_id = UNSET
        else:
            deployment_id = self.deployment_id

        field_dict: dict[str, Any] = {}
        field_dict.update(self.additional_properties)
        field_dict.update(
            {
                "request_id": request_id,
                "received_at": received_at,
                "route": route,
                "http_status": http_status,
                "error_class": error_class,
                "sample_message": sample_message,
            }
        )
        if deployment_id is not UNSET:
            field_dict["deployment_id"] = deployment_id

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        request_id = d.pop("request_id")

        received_at = datetime.datetime.fromisoformat(d.pop("received_at"))

        route = d.pop("route")

        http_status = d.pop("http_status")

        error_class = d.pop("error_class")

        sample_message = d.pop("sample_message")

        def _parse_deployment_id(data: object) -> None | str | Unset:
            if data is None:
                return data
            if isinstance(data, Unset):
                return data
            return cast(None | str | Unset, data)

        deployment_id = _parse_deployment_id(d.pop("deployment_id", UNSET))

        app_error_request_item = cls(
            request_id=request_id,
            received_at=received_at,
            route=route,
            http_status=http_status,
            error_class=error_class,
            sample_message=sample_message,
            deployment_id=deployment_id,
        )

        app_error_request_item.additional_properties = d
        return app_error_request_item

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
