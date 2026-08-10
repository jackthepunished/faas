from typing import Literal

EdgeRuleResponseKind = Literal["cors", "headers", "ip", "jwt", "redirect", "rewrite", "route"]

EDGE_RULE_RESPONSE_KIND_VALUES: set[EdgeRuleResponseKind] = {
    "cors",
    "headers",
    "ip",
    "jwt",
    "redirect",
    "rewrite",
    "route",
}


def check_edge_rule_response_kind(value: str) -> EdgeRuleResponseKind:
    if value in EDGE_RULE_RESPONSE_KIND_VALUES:
        return value
    raise TypeError(f"Unexpected value {value!r}. Expected one of {EDGE_RULE_RESPONSE_KIND_VALUES!r}")
