from typing import Literal

DeploymentResponseLastAutoRollbackReasonType2Type1 = Literal["first_window_expired", "threshold_exceeded"]

DEPLOYMENT_RESPONSE_LAST_AUTO_ROLLBACK_REASON_TYPE_2_TYPE_1_VALUES: set[
    DeploymentResponseLastAutoRollbackReasonType2Type1
] = {
    "first_window_expired",
    "threshold_exceeded",
}


def check_deployment_response_last_auto_rollback_reason_type_2_type_1(
    value: str,
) -> DeploymentResponseLastAutoRollbackReasonType2Type1:
    if value in DEPLOYMENT_RESPONSE_LAST_AUTO_ROLLBACK_REASON_TYPE_2_TYPE_1_VALUES:
        return value
    raise TypeError(
        f"Unexpected value {value!r}. Expected one of {DEPLOYMENT_RESPONSE_LAST_AUTO_ROLLBACK_REASON_TYPE_2_TYPE_1_VALUES!r}"
    )
