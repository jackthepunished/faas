from typing import Literal

BuildResponseStatus = Literal["failed", "queued", "running", "succeeded"]

BUILD_RESPONSE_STATUS_VALUES: set[BuildResponseStatus] = {
    "failed",
    "queued",
    "running",
    "succeeded",
}


def check_build_response_status(value: str) -> BuildResponseStatus:
    if value in BUILD_RESPONSE_STATUS_VALUES:
        return value
    raise TypeError(f"Unexpected value {value!r}. Expected one of {BUILD_RESPONSE_STATUS_VALUES!r}")
