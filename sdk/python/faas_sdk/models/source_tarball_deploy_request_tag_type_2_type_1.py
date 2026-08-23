from typing import Literal

SourceTarballDeployRequestTagType2Type1 = Literal[
    "compliance_hold", "hotfix", "incident_recovery", "partner_request", "scheduled_maintenance"
]

SOURCE_TARBALL_DEPLOY_REQUEST_TAG_TYPE_2_TYPE_1_VALUES: set[SourceTarballDeployRequestTagType2Type1] = {
    "compliance_hold",
    "hotfix",
    "incident_recovery",
    "partner_request",
    "scheduled_maintenance",
}


def check_source_tarball_deploy_request_tag_type_2_type_1(value: str) -> SourceTarballDeployRequestTagType2Type1:
    if value in SOURCE_TARBALL_DEPLOY_REQUEST_TAG_TYPE_2_TYPE_1_VALUES:
        return value
    raise TypeError(
        f"Unexpected value {value!r}. Expected one of {SOURCE_TARBALL_DEPLOY_REQUEST_TAG_TYPE_2_TYPE_1_VALUES!r}"
    )
