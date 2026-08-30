from __future__ import annotations

import datetime
from collections.abc import Mapping
from typing import TYPE_CHECKING, Any, TypeVar, cast
from uuid import UUID

from attrs import define as _attrs_define
from attrs import field as _attrs_field

from ..models.job_response_kind import JobResponseKind, check_job_response_kind
from ..models.job_response_status import JobResponseStatus, check_job_response_status
from ..types import UNSET, Unset

if TYPE_CHECKING:
    from ..models.job_response_env_overrides import JobResponseEnvOverrides


T = TypeVar("T", bound="JobResponse")


@_attrs_define
class JobResponse:
    """Wire projection of state.Job."""

    id: UUID
    account_id: UUID
    name: str
    kind: JobResponseKind
    image_ref: str
    command: list[str]
    ram_mb: int
    task_timeout_sec: int
    max_parallelism: int
    retry_max: int
    status: JobResponseStatus
    created_at: datetime.datetime
    updated_at: datetime.datetime
    env_overrides: JobResponseEnvOverrides | Unset = UNSET
    additional_properties: dict[str, Any] = _attrs_field(init=False, factory=dict)

    def to_dict(self) -> dict[str, Any]:
        id = str(self.id)

        account_id = str(self.account_id)

        name = self.name

        kind: str = self.kind

        image_ref = self.image_ref

        command = self.command

        ram_mb = self.ram_mb

        task_timeout_sec = self.task_timeout_sec

        max_parallelism = self.max_parallelism

        retry_max = self.retry_max

        status: str = self.status

        created_at = self.created_at.isoformat()

        updated_at = self.updated_at.isoformat()

        env_overrides: dict[str, Any] | Unset = UNSET
        if not isinstance(self.env_overrides, Unset):
            env_overrides = self.env_overrides.to_dict()

        field_dict: dict[str, Any] = {}
        field_dict.update(self.additional_properties)
        field_dict.update(
            {
                "id": id,
                "account_id": account_id,
                "name": name,
                "kind": kind,
                "image_ref": image_ref,
                "command": command,
                "ram_mb": ram_mb,
                "task_timeout_sec": task_timeout_sec,
                "max_parallelism": max_parallelism,
                "retry_max": retry_max,
                "status": status,
                "created_at": created_at,
                "updated_at": updated_at,
            }
        )
        if env_overrides is not UNSET:
            field_dict["env_overrides"] = env_overrides

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        from ..models.job_response_env_overrides import JobResponseEnvOverrides

        d = dict(src_dict)
        id = UUID(d.pop("id"))

        account_id = UUID(d.pop("account_id"))

        name = d.pop("name")

        kind = check_job_response_kind(d.pop("kind"))

        image_ref = d.pop("image_ref")

        command = cast(list[str], d.pop("command"))

        ram_mb = d.pop("ram_mb")

        task_timeout_sec = d.pop("task_timeout_sec")

        max_parallelism = d.pop("max_parallelism")

        retry_max = d.pop("retry_max")

        status = check_job_response_status(d.pop("status"))

        created_at = datetime.datetime.fromisoformat(d.pop("created_at"))

        updated_at = datetime.datetime.fromisoformat(d.pop("updated_at"))

        _env_overrides = d.pop("env_overrides", UNSET)
        env_overrides: JobResponseEnvOverrides | Unset
        if isinstance(_env_overrides, Unset):
            env_overrides = UNSET
        else:
            env_overrides = JobResponseEnvOverrides.from_dict(_env_overrides)

        job_response = cls(
            id=id,
            account_id=account_id,
            name=name,
            kind=kind,
            image_ref=image_ref,
            command=command,
            ram_mb=ram_mb,
            task_timeout_sec=task_timeout_sec,
            max_parallelism=max_parallelism,
            retry_max=retry_max,
            status=status,
            created_at=created_at,
            updated_at=updated_at,
            env_overrides=env_overrides,
        )

        job_response.additional_properties = d
        return job_response

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
