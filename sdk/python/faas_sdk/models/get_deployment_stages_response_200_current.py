from typing import Literal

GetDeploymentStagesResponse200Current = Literal[
    "dependency_restore", "image_build", "readiness", "security_scan", "snapshot_prepare", "source_download"
]

GET_DEPLOYMENT_STAGES_RESPONSE_200_CURRENT_VALUES: set[GetDeploymentStagesResponse200Current] = {
    "dependency_restore",
    "image_build",
    "readiness",
    "security_scan",
    "snapshot_prepare",
    "source_download",
}


def check_get_deployment_stages_response_200_current(value: str) -> GetDeploymentStagesResponse200Current:
    if value in GET_DEPLOYMENT_STAGES_RESPONSE_200_CURRENT_VALUES:
        return value
    raise TypeError(
        f"Unexpected value {value!r}. Expected one of {GET_DEPLOYMENT_STAGES_RESPONSE_200_CURRENT_VALUES!r}"
    )
