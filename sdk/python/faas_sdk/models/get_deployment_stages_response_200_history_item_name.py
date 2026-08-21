from typing import Literal

GetDeploymentStagesResponse200HistoryItemName = Literal[
    "dependency_restore", "image_build", "readiness", "security_scan", "snapshot_prepare", "source_download"
]

GET_DEPLOYMENT_STAGES_RESPONSE_200_HISTORY_ITEM_NAME_VALUES: set[GetDeploymentStagesResponse200HistoryItemName] = {
    "dependency_restore",
    "image_build",
    "readiness",
    "security_scan",
    "snapshot_prepare",
    "source_download",
}


def check_get_deployment_stages_response_200_history_item_name(
    value: str,
) -> GetDeploymentStagesResponse200HistoryItemName:
    if value in GET_DEPLOYMENT_STAGES_RESPONSE_200_HISTORY_ITEM_NAME_VALUES:
        return value
    raise TypeError(
        f"Unexpected value {value!r}. Expected one of {GET_DEPLOYMENT_STAGES_RESPONSE_200_HISTORY_ITEM_NAME_VALUES!r}"
    )
