from __future__ import annotations

from collections.abc import Mapping
from typing import TYPE_CHECKING, Any, TypeVar

from attrs import define as _attrs_define
from attrs import field as _attrs_field

from ..types import UNSET, Unset

if TYPE_CHECKING:
    from ..models.create_job_run_request_env_overrides import CreateJobRunRequestEnvOverrides


T = TypeVar("T", bound="CreateJobRunRequest")


@_attrs_define
class CreateJobRunRequest:
    """Atomic fan-out via `generate_series` CTE in pgstore; the
    handler validates `tasks` against `Plan.JobMaxTasksPerRun`
    (Hobby=100, Pro=1000, Scale=5000). Per-run overrides
    (parallelism / retry_max / task_timeout_sec) inherit from
    the job when null.

    """

    tasks: int
    parallelism: int | Unset = UNSET
    retry_max: int | Unset = UNSET
    task_timeout_sec: int | Unset = UNSET
    env_overrides: CreateJobRunRequestEnvOverrides | Unset = UNSET
    additional_properties: dict[str, Any] = _attrs_field(init=False, factory=dict)

    def to_dict(self) -> dict[str, Any]:
        tasks = self.tasks

        parallelism = self.parallelism

        retry_max = self.retry_max

        task_timeout_sec = self.task_timeout_sec

        env_overrides: dict[str, Any] | Unset = UNSET
        if not isinstance(self.env_overrides, Unset):
            env_overrides = self.env_overrides.to_dict()

        field_dict: dict[str, Any] = {}
        field_dict.update(self.additional_properties)
        field_dict.update(
            {
                "tasks": tasks,
            }
        )
        if parallelism is not UNSET:
            field_dict["parallelism"] = parallelism
        if retry_max is not UNSET:
            field_dict["retry_max"] = retry_max
        if task_timeout_sec is not UNSET:
            field_dict["task_timeout_sec"] = task_timeout_sec
        if env_overrides is not UNSET:
            field_dict["env_overrides"] = env_overrides

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        from ..models.create_job_run_request_env_overrides import CreateJobRunRequestEnvOverrides

        d = dict(src_dict)
        tasks = d.pop("tasks")

        parallelism = d.pop("parallelism", UNSET)

        retry_max = d.pop("retry_max", UNSET)

        task_timeout_sec = d.pop("task_timeout_sec", UNSET)

        _env_overrides = d.pop("env_overrides", UNSET)
        env_overrides: CreateJobRunRequestEnvOverrides | Unset
        if isinstance(_env_overrides, Unset):
            env_overrides = UNSET
        else:
            env_overrides = CreateJobRunRequestEnvOverrides.from_dict(_env_overrides)

        create_job_run_request = cls(
            tasks=tasks,
            parallelism=parallelism,
            retry_max=retry_max,
            task_timeout_sec=task_timeout_sec,
            env_overrides=env_overrides,
        )

        create_job_run_request.additional_properties = d
        return create_job_run_request

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
