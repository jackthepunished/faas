from typing import Literal

CreateAppWebhookRequestEventFilterItem = Literal[
    "app.created", "app.deleted", "build.failed", "build.succeeded", "cron.fired"
]

CREATE_APP_WEBHOOK_REQUEST_EVENT_FILTER_ITEM_VALUES: set[CreateAppWebhookRequestEventFilterItem] = {
    "app.created",
    "app.deleted",
    "build.failed",
    "build.succeeded",
    "cron.fired",
}


def check_create_app_webhook_request_event_filter_item(value: str) -> CreateAppWebhookRequestEventFilterItem:
    if value in CREATE_APP_WEBHOOK_REQUEST_EVENT_FILTER_ITEM_VALUES:
        return value
    raise TypeError(
        f"Unexpected value {value!r}. Expected one of {CREATE_APP_WEBHOOK_REQUEST_EVENT_FILTER_ITEM_VALUES!r}"
    )
