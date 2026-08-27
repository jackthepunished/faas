from typing import Literal

AlertPresetResponseCategory = Literal["availability", "cost", "deployment", "infrastructure", "reliability"]

ALERT_PRESET_RESPONSE_CATEGORY_VALUES: set[AlertPresetResponseCategory] = {
    "availability",
    "cost",
    "deployment",
    "infrastructure",
    "reliability",
}


def check_alert_preset_response_category(value: str) -> AlertPresetResponseCategory:
    if value in ALERT_PRESET_RESPONSE_CATEGORY_VALUES:
        return value
    raise TypeError(f"Unexpected value {value!r}. Expected one of {ALERT_PRESET_RESPONSE_CATEGORY_VALUES!r}")
