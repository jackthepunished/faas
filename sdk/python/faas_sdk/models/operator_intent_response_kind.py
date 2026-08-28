from typing import Literal

OperatorIntentResponseKind = Literal["force_cold_boot", "force_park", "force_restart"]

OPERATOR_INTENT_RESPONSE_KIND_VALUES: set[OperatorIntentResponseKind] = {
    "force_cold_boot",
    "force_park",
    "force_restart",
}


def check_operator_intent_response_kind(value: str) -> OperatorIntentResponseKind:
    if value in OPERATOR_INTENT_RESPONSE_KIND_VALUES:
        return value
    raise TypeError(f"Unexpected value {value!r}. Expected one of {OPERATOR_INTENT_RESPONSE_KIND_VALUES!r}")
