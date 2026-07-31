from __future__ import annotations

from collections.abc import Mapping
from typing import TYPE_CHECKING, Any, TypeVar, cast

from attrs import define as _attrs_define
from attrs import field as _attrs_field

from ..types import UNSET, Unset

if TYPE_CHECKING:
    from ..models.apply_response_apps_item import ApplyResponseAppsItem
    from ..models.plan_cron import PlanCron
    from ..models.plan_managed import PlanManaged
    from ..models.plan_workload import PlanWorkload


T = TypeVar("T", bound="ApplyResponse")


@_attrs_define
class ApplyResponse:
    """Apply response. Carries the inserted project_id and per-app IDs."""

    project_slug: str
    scan_source: str
    can_apply: bool
    plan_token: str
    repo_full_name: str | Unset = UNSET
    tier: str | Unset = UNSET
    workloads: list[PlanWorkload] | Unset = UNSET
    managed: list[PlanManaged] | Unset = UNSET
    crons: list[PlanCron] | Unset = UNSET
    warnings: list[str] | Unset = UNSET
    observed_apps: int | Unset = UNSET
    observed_crons: int | Unset = UNSET
    limit_apps: int | Unset = UNSET
    limit_crons: int | Unset = UNSET
    crons_not_allowed: bool | Unset = UNSET
    project_id: str | Unset = UNSET
    apps: list[ApplyResponseAppsItem] | Unset = UNSET
    additional_properties: dict[str, Any] = _attrs_field(init=False, factory=dict)

    def to_dict(self) -> dict[str, Any]:
        project_slug = self.project_slug

        scan_source = self.scan_source

        can_apply = self.can_apply

        plan_token = self.plan_token

        repo_full_name = self.repo_full_name

        tier = self.tier

        workloads: list[dict[str, Any]] | Unset = UNSET
        if not isinstance(self.workloads, Unset):
            workloads = []
            for workloads_item_data in self.workloads:
                workloads_item = workloads_item_data.to_dict()
                workloads.append(workloads_item)

        managed: list[dict[str, Any]] | Unset = UNSET
        if not isinstance(self.managed, Unset):
            managed = []
            for managed_item_data in self.managed:
                managed_item = managed_item_data.to_dict()
                managed.append(managed_item)

        crons: list[dict[str, Any]] | Unset = UNSET
        if not isinstance(self.crons, Unset):
            crons = []
            for crons_item_data in self.crons:
                crons_item = crons_item_data.to_dict()
                crons.append(crons_item)

        warnings: list[str] | Unset = UNSET
        if not isinstance(self.warnings, Unset):
            warnings = self.warnings

        observed_apps = self.observed_apps

        observed_crons = self.observed_crons

        limit_apps = self.limit_apps

        limit_crons = self.limit_crons

        crons_not_allowed = self.crons_not_allowed

        project_id = self.project_id

        apps: list[dict[str, Any]] | Unset = UNSET
        if not isinstance(self.apps, Unset):
            apps = []
            for apps_item_data in self.apps:
                apps_item = apps_item_data.to_dict()
                apps.append(apps_item)

        field_dict: dict[str, Any] = {}
        field_dict.update(self.additional_properties)
        field_dict.update(
            {
                "project_slug": project_slug,
                "scan_source": scan_source,
                "can_apply": can_apply,
                "plan_token": plan_token,
            }
        )
        if repo_full_name is not UNSET:
            field_dict["repo_full_name"] = repo_full_name
        if tier is not UNSET:
            field_dict["tier"] = tier
        if workloads is not UNSET:
            field_dict["workloads"] = workloads
        if managed is not UNSET:
            field_dict["managed"] = managed
        if crons is not UNSET:
            field_dict["crons"] = crons
        if warnings is not UNSET:
            field_dict["warnings"] = warnings
        if observed_apps is not UNSET:
            field_dict["observed_apps"] = observed_apps
        if observed_crons is not UNSET:
            field_dict["observed_crons"] = observed_crons
        if limit_apps is not UNSET:
            field_dict["limit_apps"] = limit_apps
        if limit_crons is not UNSET:
            field_dict["limit_crons"] = limit_crons
        if crons_not_allowed is not UNSET:
            field_dict["crons_not_allowed"] = crons_not_allowed
        if project_id is not UNSET:
            field_dict["project_id"] = project_id
        if apps is not UNSET:
            field_dict["apps"] = apps

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        from ..models.apply_response_apps_item import ApplyResponseAppsItem
        from ..models.plan_cron import PlanCron
        from ..models.plan_managed import PlanManaged
        from ..models.plan_workload import PlanWorkload

        d = dict(src_dict)
        project_slug = d.pop("project_slug")

        scan_source = d.pop("scan_source")

        can_apply = d.pop("can_apply")

        plan_token = d.pop("plan_token")

        repo_full_name = d.pop("repo_full_name", UNSET)

        tier = d.pop("tier", UNSET)

        _workloads = d.pop("workloads", UNSET)
        workloads: list[PlanWorkload] | Unset = UNSET
        if _workloads is not UNSET:
            workloads = []
            for workloads_item_data in _workloads:
                workloads_item = PlanWorkload.from_dict(workloads_item_data)

                workloads.append(workloads_item)

        _managed = d.pop("managed", UNSET)
        managed: list[PlanManaged] | Unset = UNSET
        if _managed is not UNSET:
            managed = []
            for managed_item_data in _managed:
                managed_item = PlanManaged.from_dict(managed_item_data)

                managed.append(managed_item)

        _crons = d.pop("crons", UNSET)
        crons: list[PlanCron] | Unset = UNSET
        if _crons is not UNSET:
            crons = []
            for crons_item_data in _crons:
                crons_item = PlanCron.from_dict(crons_item_data)

                crons.append(crons_item)

        warnings = cast(list[str], d.pop("warnings", UNSET))

        observed_apps = d.pop("observed_apps", UNSET)

        observed_crons = d.pop("observed_crons", UNSET)

        limit_apps = d.pop("limit_apps", UNSET)

        limit_crons = d.pop("limit_crons", UNSET)

        crons_not_allowed = d.pop("crons_not_allowed", UNSET)

        project_id = d.pop("project_id", UNSET)

        _apps = d.pop("apps", UNSET)
        apps: list[ApplyResponseAppsItem] | Unset = UNSET
        if _apps is not UNSET:
            apps = []
            for apps_item_data in _apps:
                apps_item = ApplyResponseAppsItem.from_dict(apps_item_data)

                apps.append(apps_item)

        apply_response = cls(
            project_slug=project_slug,
            scan_source=scan_source,
            can_apply=can_apply,
            plan_token=plan_token,
            repo_full_name=repo_full_name,
            tier=tier,
            workloads=workloads,
            managed=managed,
            crons=crons,
            warnings=warnings,
            observed_apps=observed_apps,
            observed_crons=observed_crons,
            limit_apps=limit_apps,
            limit_crons=limit_crons,
            crons_not_allowed=crons_not_allowed,
            project_id=project_id,
            apps=apps,
        )

        apply_response.additional_properties = d
        return apply_response

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
