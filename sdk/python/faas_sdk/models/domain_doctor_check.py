from __future__ import annotations

import datetime
from collections.abc import Mapping
from typing import Any, TypeVar, cast

from attrs import define as _attrs_define
from attrs import field as _attrs_field

from ..types import UNSET, Unset

from .domain_doctor_check_name import (
    DomainDoctorCheckName,
    check_domain_doctor_check_name,
)
from .domain_doctor_check_status import (
    DomainDoctorCheckStatus,
    check_domain_doctor_check_status,
)

T = TypeVar("T", bound="DomainDoctorCheck")


@_attrs_define
class DomainDoctorCheck:
    """One row of the doctor report. Stable name tokens (dns_record / points_to_gregale / tls_certificate /
    caa_permits / ipv6_conflict) so the CLI can filter by name without parsing the human Detail field.
    Remediation is the exact record to change when status is fail — the load-bearing field for the
    activation drop-off.
    """

    name: DomainDoctorCheckName
    status: DomainDoctorCheckStatus
    detail: str
    observed: None | str | Unset = UNSET
    remediation: None | str | Unset = UNSET
    checked_at: datetime.datetime | None | Unset = UNSET
    additional_properties: dict[str, Any] = _attrs_field(init=False, factory=dict)

    def to_dict(self) -> dict[str, Any]:
        name = self.name

        status = self.status

        detail = self.detail

        observed: None | str | Unset
        if isinstance(self.observed, Unset):
            observed = UNSET
        else:
            observed = self.observed

        remediation: None | str | Unset
        if isinstance(self.remediation, Unset):
            remediation = UNSET
        else:
            remediation = self.remediation

        checked_at: None | str | Unset
        if isinstance(self.checked_at, Unset):
            checked_at = UNSET
        elif isinstance(self.checked_at, datetime.datetime):
            checked_at = self.checked_at.isoformat()
        else:
            checked_at = self.checked_at

        field_dict: dict[str, Any] = {}
        field_dict.update(self.additional_properties)
        field_dict.update(
            {
                "name": name,
                "status": status,
                "detail": detail,
            }
        )
        if observed is not UNSET:
            field_dict["observed"] = observed
        if remediation is not UNSET:
            field_dict["remediation"] = remediation
        if checked_at is not UNSET:
            field_dict["checked_at"] = checked_at

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        name = check_domain_doctor_check_name(d.pop("name"))

        status = check_domain_doctor_check_status(d.pop("status"))

        detail = d.pop("detail")

        def _parse_observed(data: object) -> None | str | Unset:
            if data is None:
                return data
            if isinstance(data, Unset):
                return data
            return cast(None | str | Unset, data)

        observed = _parse_observed(d.pop("observed", UNSET))

        def _parse_remediation(data: object) -> None | str | Unset:
            if data is None:
                return data
            if isinstance(data, Unset):
                return data
            return cast(None | str | Unset, data)

        remediation = _parse_remediation(d.pop("remediation", UNSET))

        def _parse_checked_at(data: object) -> datetime.datetime | None | Unset:
            if data is None:
                return data
            if isinstance(data, Unset):
                return data
            try:
                if not isinstance(data, str):
                    raise TypeError()
                checked_at_type_0 = datetime.datetime.fromisoformat(data)

                return checked_at_type_0
            except (TypeError, ValueError, AttributeError, KeyError):
                pass
            return cast(datetime.datetime | None | Unset, data)

        checked_at = _parse_checked_at(d.pop("checked_at", UNSET))

        domain_doctor_check = cls(
            name=name,
            status=status,
            detail=detail,
            observed=observed,
            remediation=remediation,
            checked_at=checked_at,
        )

        domain_doctor_check.additional_properties = d
        return domain_doctor_check

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