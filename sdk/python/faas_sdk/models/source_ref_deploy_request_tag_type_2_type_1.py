from typing import Literal

SourceRefDeployRequestTagType2Type1 = Literal[
    "compliance_hold", "hotfix", "incident_recovery", "partner_request", "scheduled_maintenance"
]

SOURCE_REF_DEPLOY_REQUEST_TAG_TYPE_2_TYPE_1_VALUES: set[SourceRefDeployRequestTagType2Type1] = {
    "compliance_hold",
    "hotfix",
    "incident_recovery",
    "partner_request",
    "scheduled_maintenance",
}


def check_source_ref_deploy_request_tag_type_2_type_1(value: str) -> SourceRefDeployRequestTagType2Type1:
    if value in SOURCE_REF_DEPLOY_REQUEST_TAG_TYPE_2_TYPE_1_VALUES:
        return value
    raise TypeError(
        f"Unexpected value {value!r}. Expected one of {SOURCE_REF_DEPLOY_REQUEST_TAG_TYPE_2_TYPE_1_VALUES!r}"
    )
