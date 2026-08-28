from typing import Literal

DeploymentAuditResponseKind = Literal[
    "deploy.created",
    "deploy.recovered",
    "deploy.rolled_back",
    "deploy.rolled_forward",
    "deploy.stuck",
    "deploy.traffic_changed",
]

DEPLOYMENT_AUDIT_RESPONSE_KIND_VALUES: set[DeploymentAuditResponseKind] = {
    "deploy.created",
    "deploy.recovered",
    "deploy.rolled_back",
    "deploy.rolled_forward",
    "deploy.stuck",
    "deploy.traffic_changed",
}


def check_deployment_audit_response_kind(value: str) -> DeploymentAuditResponseKind:
    if value in DEPLOYMENT_AUDIT_RESPONSE_KIND_VALUES:
        return value
    raise TypeError(f"Unexpected value {value!r}. Expected one of {DEPLOYMENT_AUDIT_RESPONSE_KIND_VALUES!r}")
