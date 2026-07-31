from __future__ import annotations

from collections.abc import Mapping
from typing import TYPE_CHECKING, Any, TypeVar, cast

from attrs import define as _attrs_define
from attrs import field as _attrs_field

from ..models.plan_response_scan_source import PlanResponseScanSource, check_plan_response_scan_source
from ..types import UNSET, Unset

if TYPE_CHECKING:
    from ..models.plan_cron import PlanCron
    from ..models.plan_managed import PlanManaged
    from ..models.plan_workload import PlanWorkload


T = TypeVar("T", bound="PlanResponse")


@_attrs_define
class PlanResponse:
    """Dry-run scan response."""

    project_slug: str
    scan_source: PlanResponseScanSource
    tier: str
    workloads: list[PlanWorkload]
    managed: list[PlanManaged]
    crons: list[PlanCron]
    observed_apps: int
    observed_crons: int
    limit_apps: int
    limit_crons: int
    can_apply: bool
    plan_token: str
    """base64-JSON plan token; pass back as ?plan_token= on /v1/projects to skip the second extract."""
    repo_full_name: str | Unset = UNSET
    warnings: list[str] | Unset = UNSET
    crons_not_allowed: bool | Unset = UNSET
    additional_properties: dict[str, Any] = _attrs_field(init=False, factory=dict)

    def to_dict(self) -> dict[str, Any]:
        project_slug = self.project_slug

        scan_source: str = self.scan_source

        tier = self.tier

        workloads = []
        for workloads_item_data in self.workloads:
            workloads_item = workloads_item_data.to_dict()
            workloads.append(workloads_item)

        managed = []
        for managed_item_data in self.managed:
            managed_item = managed_item_data.to_dict()
            managed.append(managed_item)

        crons = []
        for crons_item_data in self.crons:
            crons_item = crons_item_data.to_dict()
            crons.append(crons_item)

        observed_apps = self.observed_apps

        observed_crons = self.observed_crons

        limit_apps = self.limit_apps

        limit_crons = self.limit_crons

        can_apply = self.can_apply

        plan_token = self.plan_token

        repo_full_name = self.repo_full_name

        warnings: list[str] | Unset = UNSET
        if not isinstance(self.warnings, Unset):
            warnings = self.warnings

        crons_not_allowed = self.crons_not_allowed

        field_dict: dict[str, Any] = {}
        field_dict.update(self.additional_properties)
        field_dict.update(
            {
                "project_slug": project_slug,
                "scan_source": scan_source,
                "tier": tier,
                "workloads": workloads,
                "managed": managed,
                "crons": crons,
                "observed_apps": observed_apps,
                "observed_crons": observed_crons,
                "limit_apps": limit_apps,
                "limit_crons": limit_crons,
                "can_apply": can_apply,
                "plan_token": plan_token,
            }
        )
        if repo_full_name is not UNSET:
            field_dict["repo_full_name"] = repo_full_name
        if warnings is not UNSET:
            field_dict["warnings"] = warnings
        if crons_not_allowed is not UNSET:
            field_dict["crons_not_allowed"] = crons_not_allowed

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        from ..models.plan_cron import PlanCron
        from ..models.plan_managed import PlanManaged
        from ..models.plan_workload import PlanWorkload

        d = dict(src_dict)
        project_slug = d.pop("project_slug")

        scan_source = check_plan_response_scan_source(d.pop("scan_source"))

        tier = d.pop("tier")

        workloads = []
        _workloads = d.pop("workloads")
        for workloads_item_data in _workloads:
            workloads_item = PlanWorkload.from_dict(workloads_item_data)

            workloads.append(workloads_item)

        managed = []
        _managed = d.pop("managed")
        for managed_item_data in _managed:
            managed_item = PlanManaged.from_dict(managed_item_data)

            managed.append(managed_item)

        crons = []
        _crons = d.pop("crons")
        for crons_item_data in _crons:
            crons_item = PlanCron.from_dict(crons_item_data)

            crons.append(crons_item)

        observed_apps = d.pop("observed_apps")

        observed_crons = d.pop("observed_crons")

        limit_apps = d.pop("limit_apps")

        limit_crons = d.pop("limit_crons")

        can_apply = d.pop("can_apply")

        plan_token = d.pop("plan_token")

        repo_full_name = d.pop("repo_full_name", UNSET)

        warnings = cast(list[str], d.pop("warnings", UNSET))

        crons_not_allowed = d.pop("crons_not_allowed", UNSET)

        plan_response = cls(
            project_slug=project_slug,
            scan_source=scan_source,
            tier=tier,
            workloads=workloads,
            managed=managed,
            crons=crons,
            observed_apps=observed_apps,
            observed_crons=observed_crons,
            limit_apps=limit_apps,
            limit_crons=limit_crons,
            can_apply=can_apply,
            plan_token=plan_token,
            repo_full_name=repo_full_name,
            warnings=warnings,
            crons_not_allowed=crons_not_allowed,
        )

        plan_response.additional_properties = d
        return plan_response

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
