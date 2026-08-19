from typing import Literal

DomainDoctorCheckName = Literal[
    "dns_record",
    "points_to_gregale",
    "tls_certificate",
    "caa_permits",
    "ipv6_conflict",
]

DOMAIN_DOCTOR_CHECK_NAME_VALUES: set[DomainDoctorCheckName] = {
    "dns_record",
    "points_to_gregale",
    "tls_certificate",
    "caa_permits",
    "ipv6_conflict",
}


def check_domain_doctor_check_name(value: str) -> DomainDoctorCheckName:
    if value in DOMAIN_DOCTOR_CHECK_NAME_VALUES:
        return value
    raise TypeError(f"Unexpected value {value!r}. Expected one of {DOMAIN_DOCTOR_CHECK_NAME_VALUES!r}")