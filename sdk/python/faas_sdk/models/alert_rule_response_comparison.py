from typing import Literal

AlertRuleResponseComparison = Literal["gt", "gte", "lt", "lte"]

ALERT_RULE_RESPONSE_COMPARISON_VALUES: set[AlertRuleResponseComparison] = {
    "gt",
    "gte",
    "lt",
    "lte",
}


def check_alert_rule_response_comparison(value: str) -> AlertRuleResponseComparison:
    if value in ALERT_RULE_RESPONSE_COMPARISON_VALUES:
        return value
    raise TypeError(f"Unexpected value {value!r}. Expected one of {ALERT_RULE_RESPONSE_COMPARISON_VALUES!r}")
