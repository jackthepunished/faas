from typing import Literal

UpdateAppWebhookRequestEventFilterItem = Literal[
    "app.created", "app.deleted", "build.failed", "build.succeeded", "cron.fired"
]

UPDATE_APP_WEBHOOK_REQUEST_EVENT_FILTER_ITEM_VALUES: set[UpdateAppWebhookRequestEventFilterItem] = {
    "app.created",
    "app.deleted",
    "build.failed",
    "build.succeeded",
    "cron.fired",
}


def check_update_app_webhook_request_event_filter_item(value: str) -> UpdateAppWebhookRequestEventFilterItem:
    if value in UPDATE_APP_WEBHOOK_REQUEST_EVENT_FILTER_ITEM_VALUES:
        return value
    raise TypeError(
        f"Unexpected value {value!r}. Expected one of {UPDATE_APP_WEBHOOK_REQUEST_EVENT_FILTER_ITEM_VALUES!r}"
    )
