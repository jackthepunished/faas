from typing import Literal

CreateEdgeRuleRequestKind = Literal["cors", "headers", "ip", "jwt", "redirect", "rewrite", "route"]

CREATE_EDGE_RULE_REQUEST_KIND_VALUES: set[CreateEdgeRuleRequestKind] = {
    "cors",
    "headers",
    "ip",
    "jwt",
    "redirect",
    "rewrite",
    "route",
}


def check_create_edge_rule_request_kind(value: str) -> CreateEdgeRuleRequestKind:
    if value in CREATE_EDGE_RULE_REQUEST_KIND_VALUES:
        return value
    raise TypeError(f"Unexpected value {value!r}. Expected one of {CREATE_EDGE_RULE_REQUEST_KIND_VALUES!r}")
