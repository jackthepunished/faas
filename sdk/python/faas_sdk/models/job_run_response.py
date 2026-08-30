from __future__ import annotations

import datetime
from collections.abc import Mapping
from typing import TYPE_CHECKING, Any, TypeVar
from uuid import UUID

from attrs import define as _attrs_define
from attrs import field as _attrs_field

from ..models.job_run_response_aggregate_status import (
    JobRunResponseAggregateStatus,
    check_job_run_response_aggregate_status,
)
from ..models.job_run_response_trigger_kind import JobRunResponseTriggerKind, check_job_run_response_trigger_kind
from ..types import UNSET, Unset

if TYPE_CHECKING:
    from ..models.job_run_response_env_overrides import JobRunResponseEnvOverrides


T = TypeVar("T", bound="JobRunResponse")


@_attrs_define
class JobRunResponse:
    """Wire projection of state.JobRun. Aggregate counters are recomputed by schedd after every terminal task transition."""

    id: UUID
    job_id: UUID
    account_id: UUID
    trigger_kind: JobRunResponseTriggerKind
    tasks: int
    parallelism: int
    aggregate_status: JobRunResponseAggregateStatus
    tasks_succeeded: int
    tasks_failed: int
    tasks_cancelled: int
    tasks_running: int
    dead_letter_count: int
    created_at: datetime.datetime
    env_overrides: JobRunResponseEnvOverrides | Unset = UNSET
    retry_max: int | Unset = UNSET
    task_timeout_sec: int | Unset = UNSET
    started_at: datetime.datetime | Unset = UNSET
    finished_at: datetime.datetime | Unset = UNSET
    additional_properties: dict[str, Any] = _attrs_field(init=False, factory=dict)

    def to_dict(self) -> dict[str, Any]:
        id = str(self.id)

        job_id = str(self.job_id)

        account_id = str(self.account_id)

        trigger_kind: str = self.trigger_kind

        tasks = self.tasks

        parallelism = self.parallelism

        aggregate_status: str = self.aggregate_status

        tasks_succeeded = self.tasks_succeeded

        tasks_failed = self.tasks_failed

        tasks_cancelled = self.tasks_cancelled

        tasks_running = self.tasks_running

        dead_letter_count = self.dead_letter_count

        created_at = self.created_at.isoformat()

        env_overrides: dict[str, Any] | Unset = UNSET
        if not isinstance(self.env_overrides, Unset):
            env_overrides = self.env_overrides.to_dict()

        retry_max = self.retry_max

        task_timeout_sec = self.task_timeout_sec

        started_at: str | Unset = UNSET
        if not isinstance(self.started_at, Unset):
            started_at = self.started_at.isoformat()

        finished_at: str | Unset = UNSET
        if not isinstance(self.finished_at, Unset):
            finished_at = self.finished_at.isoformat()

        field_dict: dict[str, Any] = {}
        field_dict.update(self.additional_properties)
        field_dict.update(
            {
                "id": id,
                "job_id": job_id,
                "account_id": account_id,
                "trigger_kind": trigger_kind,
                "tasks": tasks,
                "parallelism": parallelism,
                "aggregate_status": aggregate_status,
                "tasks_succeeded": tasks_succeeded,
                "tasks_failed": tasks_failed,
                "tasks_cancelled": tasks_cancelled,
                "tasks_running": tasks_running,
                "dead_letter_count": dead_letter_count,
                "created_at": created_at,
            }
        )
        if env_overrides is not UNSET:
            field_dict["env_overrides"] = env_overrides
        if retry_max is not UNSET:
            field_dict["retry_max"] = retry_max
        if task_timeout_sec is not UNSET:
            field_dict["task_timeout_sec"] = task_timeout_sec
        if started_at is not UNSET:
            field_dict["started_at"] = started_at
        if finished_at is not UNSET:
            field_dict["finished_at"] = finished_at

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        from ..models.job_run_response_env_overrides import JobRunResponseEnvOverrides

        d = dict(src_dict)
        id = UUID(d.pop("id"))

        job_id = UUID(d.pop("job_id"))

        account_id = UUID(d.pop("account_id"))

        trigger_kind = check_job_run_response_trigger_kind(d.pop("trigger_kind"))

        tasks = d.pop("tasks")

        parallelism = d.pop("parallelism")

        aggregate_status = check_job_run_response_aggregate_status(d.pop("aggregate_status"))

        tasks_succeeded = d.pop("tasks_succeeded")

        tasks_failed = d.pop("tasks_failed")

        tasks_cancelled = d.pop("tasks_cancelled")

        tasks_running = d.pop("tasks_running")

        dead_letter_count = d.pop("dead_letter_count")

        created_at = datetime.datetime.fromisoformat(d.pop("created_at"))

        _env_overrides = d.pop("env_overrides", UNSET)
        env_overrides: JobRunResponseEnvOverrides | Unset
        if isinstance(_env_overrides, Unset):
            env_overrides = UNSET
        else:
            env_overrides = JobRunResponseEnvOverrides.from_dict(_env_overrides)

        retry_max = d.pop("retry_max", UNSET)

        task_timeout_sec = d.pop("task_timeout_sec", UNSET)

        _started_at = d.pop("started_at", UNSET)
        started_at: datetime.datetime | Unset
        if isinstance(_started_at, Unset):
            started_at = UNSET
        else:
            started_at = datetime.datetime.fromisoformat(_started_at)

        _finished_at = d.pop("finished_at", UNSET)
        finished_at: datetime.datetime | Unset
        if isinstance(_finished_at, Unset):
            finished_at = UNSET
        else:
            finished_at = datetime.datetime.fromisoformat(_finished_at)

        job_run_response = cls(
            id=id,
            job_id=job_id,
            account_id=account_id,
            trigger_kind=trigger_kind,
            tasks=tasks,
            parallelism=parallelism,
            aggregate_status=aggregate_status,
            tasks_succeeded=tasks_succeeded,
            tasks_failed=tasks_failed,
            tasks_cancelled=tasks_cancelled,
            tasks_running=tasks_running,
            dead_letter_count=dead_letter_count,
            created_at=created_at,
            env_overrides=env_overrides,
            retry_max=retry_max,
            task_timeout_sec=task_timeout_sec,
            started_at=started_at,
            finished_at=finished_at,
        )

        job_run_response.additional_properties = d
        return job_run_response

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
