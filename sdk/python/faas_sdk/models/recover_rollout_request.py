from __future__ import annotations

from collections.abc import Mapping
from typing import Any, TypeVar

from attrs import define as _attrs_define
from attrs import field as _attrs_field

from ..models.recover_rollout_request_action import RecoverRolloutRequestAction, check_recover_rollout_request_action
from ..types import UNSET, Unset

T = TypeVar("T", bound="RecoverRolloutRequest")


@_attrs_define
class RecoverRolloutRequest:
    """Body for POST /v1/apps/{slug}/rollouts/recover (SAFE-RELEASES-R, issue #976 / ADR-122). Closed-set `action` ∈
    {advance, promote, abort}; `reason` is the operator-supplied free-text captured into the deployment_audit row's data
    payload.

    """

    action: RecoverRolloutRequestAction
    """The recovery action. `advance` requires the rollout to be stuck (>30 min in the same canary step); `promote`
    short-circuits the rollout to 100% / complete; `abort` flips rollout_state='aborted'."""
    reason: str | Unset = UNSET
    """Operator-supplied reason (≤1024 chars). Lands verbatim in the deployment_audit row's data payload under the
    `reason` key."""
    additional_properties: dict[str, Any] = _attrs_field(init=False, factory=dict)

    def to_dict(self) -> dict[str, Any]:
        action: str = self.action

        reason = self.reason

        field_dict: dict[str, Any] = {}
        field_dict.update(self.additional_properties)
        field_dict.update(
            {
                "action": action,
            }
        )
        if reason is not UNSET:
            field_dict["reason"] = reason

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        action = check_recover_rollout_request_action(d.pop("action"))

        reason = d.pop("reason", UNSET)

        recover_rollout_request = cls(
            action=action,
            reason=reason,
        )

        recover_rollout_request.additional_properties = d
        return recover_rollout_request

    @property
    def additional_keys(self) -> list[str]:
        return list(self.additional_properties.keys())

    def __getitem__(self, key: str) -> Any:
        return self.additional_properties[key]

    def __setitem__(self, key: str, value: Any) -> None:
        self.additional_properties[key] = value

    def __delitem__(self, key: str) -> None:
        del self.additional_properties[key]

    def __contains__(self, key: str) -> bool:
        return key in self.additional_properties
