from typing import Literal

DomainDoctorCheckStatus = Literal["ok", "fail", "pending", "na"]

DOMAIN_DOCTOR_CHECK_STATUS_VALUES: set[DomainDoctorCheckStatus] = {
    "ok",
    "fail",
    "pending",
    "na",
}


def check_domain_doctor_check_status(value: str) -> DomainDoctorCheckStatus:
    if value in DOMAIN_DOCTOR_CHECK_STATUS_VALUES:
        return value
    raise TypeError(f"Unexpected value {value!r}. Expected one of {DOMAIN_DOCTOR_CHECK_STATUS_VALUES!r}")