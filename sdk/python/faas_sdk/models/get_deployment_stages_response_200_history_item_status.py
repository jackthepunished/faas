from typing import Literal

GetDeploymentStagesResponse200HistoryItemStatus = Literal["completed", "failed"]

GET_DEPLOYMENT_STAGES_RESPONSE_200_HISTORY_ITEM_STATUS_VALUES: set[GetDeploymentStagesResponse200HistoryItemStatus] = {
    "completed",
    "failed",
}


def check_get_deployment_stages_response_200_history_item_status(
    value: str,
) -> GetDeploymentStagesResponse200HistoryItemStatus:
    if value in GET_DEPLOYMENT_STAGES_RESPONSE_200_HISTORY_ITEM_STATUS_VALUES:
        return value
    raise TypeError(
        f"Unexpected value {value!r}. Expected one of {GET_DEPLOYMENT_STAGES_RESPONSE_200_HISTORY_ITEM_STATUS_VALUES!r}"
    )
