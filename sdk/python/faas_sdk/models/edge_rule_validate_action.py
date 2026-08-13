from __future__ import annotations

from collections.abc import Mapping
from typing import TYPE_CHECKING, Any, TypeVar, cast

from attrs import define as _attrs_define
from attrs import field as _attrs_field

from ..types import UNSET, Unset

if TYPE_CHECKING:
    from ..models.edge_rule_validate_action_schema import EdgeRuleValidateActionSchema


T = TypeVar("T", bound="EdgeRuleValidateAction")


@_attrs_define
class EdgeRuleValidateAction:
    """Customer-supplied JSON Schema (Draft 2020-12) evaluated against
    the inbound request body BEFORE the wake gate fires. The
    kind=validate rule is the platform's API-native request-
    validation surface: rejections return 422
    `request_validation_failed` with `Problem.errors` carrying
    per-field detail, never pay a cold-boot cost, never consume
    the rate-limit / wake-quota budget on malformed traffic.

    Free-and-above (no plan gate). Schema lives inline in the
    `action` jsonb blob (single-table flow), capped at the
    platform's `MaxEdgeRuleValidateSchemaBytes` (64 KiB). The
    gateway re-validates at compile time as defence-in-depth so
    the SQL hotfix path that bypasses apid-Validate still cannot
    ship an external `$ref`.

    Field-by-field:
      * `schema` — required JSON Schema document (Draft 2020-12).
        Capped at 64 KiB. External `$ref` / `$id` URLs are
        rejected at create-time; internal pointers (`#/definitions/Foo`)
        pass through.
      * `content_types` — optional media-type allowlist.
        Closed set `application/*`. Empty = match any.
      * `apply_while_streaming` — per-rule opt-in for the
        streaming response path (ADR-047). Default false mirrors
        the §4.1 `Accept: application/json` opt-out.
      * `reject_on_unknown_fields` — toggles
        `additionalProperties: false` on the compiled schema.
        Default false preserves byte-stable schemas.
      * `max_body_bytes` — per-rule inbound body cap. 0 =
        inherit `MaxRequestBodyBytes` (per-plan 25 MB buffered /
        100 MB streaming). Must be > 0 and <= `MaxRequestBodyBytes`.

    """

    schema: EdgeRuleValidateActionSchema
    """Inline JSON Schema document (Draft 2020-12). The schema
    is preserved byte-exact across apid↔gatewayd round-trips
    so the SHA-256 cache key in `pkg/edgevalidate` is stable.
    """
    content_types: list[str] | Unset = UNSET
    """Optional media-type allowlist. Every entry must start
    with `application/`. Empty array = match any Content-Type.
    """
    apply_while_streaming: bool | Unset = UNSET
    """Whether validation fires on the streaming response path
    (ADR-047). Default false; set true per-rule to opt the
    SSE / chunked response path into body validation.
    """
    reject_on_unknown_fields: bool | Unset = UNSET
    """Toggles `additionalProperties: false` on the compiled
    schema. Default false so a body with stray fields does
    not silently fail; opt in per-rule for strict schemas.
    """
    max_body_bytes: int | Unset = UNSET
    """Per-rule inbound body cap. 0 (default) inherits the
    platform cap (`api.MaxRequestBodyBytes`). When set, must
    be > 0 and <= the platform cap.
    """
    additional_properties: dict[str, Any] = _attrs_field(init=False, factory=dict)

    def to_dict(self) -> dict[str, Any]:
        schema = self.schema.to_dict()

        content_types: list[str] | Unset = UNSET
        if not isinstance(self.content_types, Unset):
            content_types = self.content_types

        apply_while_streaming = self.apply_while_streaming

        reject_on_unknown_fields = self.reject_on_unknown_fields

        max_body_bytes = self.max_body_bytes

        field_dict: dict[str, Any] = {}
        field_dict.update(self.additional_properties)
        field_dict.update(
            {
                "schema": schema,
            }
        )
        if content_types is not UNSET:
            field_dict["content_types"] = content_types
        if apply_while_streaming is not UNSET:
            field_dict["apply_while_streaming"] = apply_while_streaming
        if reject_on_unknown_fields is not UNSET:
            field_dict["reject_on_unknown_fields"] = reject_on_unknown_fields
        if max_body_bytes is not UNSET:
            field_dict["max_body_bytes"] = max_body_bytes

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        from ..models.edge_rule_validate_action_schema import EdgeRuleValidateActionSchema

        d = dict(src_dict)
        schema = EdgeRuleValidateActionSchema.from_dict(d.pop("schema"))

        content_types = cast(list[str], d.pop("content_types", UNSET))

        apply_while_streaming = d.pop("apply_while_streaming", UNSET)

        reject_on_unknown_fields = d.pop("reject_on_unknown_fields", UNSET)

        max_body_bytes = d.pop("max_body_bytes", UNSET)

        edge_rule_validate_action = cls(
            schema=schema,
            content_types=content_types,
            apply_while_streaming=apply_while_streaming,
            reject_on_unknown_fields=reject_on_unknown_fields,
            max_body_bytes=max_body_bytes,
        )

        edge_rule_validate_action.additional_properties = d
        return edge_rule_validate_action

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
