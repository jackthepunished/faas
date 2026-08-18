from __future__ import annotations

import datetime
from collections.abc import Mapping
from typing import Any, TypeVar
from uuid import UUID

from attrs import define as _attrs_define
from attrs import field as _attrs_field

from ..models.data_upstream_response_kind import DataUpstreamResponseKind, check_data_upstream_response_kind
from ..models.data_upstream_response_source import DataUpstreamResponseSource, check_data_upstream_response_source
from ..types import UNSET, Unset

T = TypeVar("T", bound="DataUpstreamResponse")


@_attrs_define
class DataUpstreamResponse:
    """A single customer data upstream. The plaintext host is replaced by
    `host_redacted_hash` (sha256(salt||host) 8-hex prefix); the §11
    barrier means the wire format never carries the customer's DSN.

    """

    id: UUID
    source: DataUpstreamResponseSource
    """Whether the row was captured by the classifier (FAAS_DATA_PLACEMENT=1) or added via PUT (explicit)."""
    kind: DataUpstreamResponseKind
    host_redacted_hash: str
    """SHA-256 hex of (HostHashSalt||host). 64 lowercase hex chars, matching the schema CHECK constraint."""
    port: int
    created_at: datetime.datetime
    last_seen_at: datetime.datetime
    host_last4: str | Unset = UNSET
    """First 8 hex chars of host_redacted_hash; safe for log/scrape correlation (8 chars = ~4B capacity)."""
    scope: str | Unset = UNSET
    """ADR-090 deployment-scope filter (3..40 chars, lowercase alnum + dash). Echoes the value persisted on the
    row; absent when the default scope applies."""
    declared_region: str | Unset = UNSET
    """Region hint (nullable). Empty on capture; populated by the operator or the classify-flow follow-up."""
    last_rtt_ms: int | Unset = UNSET
    """Most recent probe RTT (ms). Omitted when no probe yet."""
    last_probed_at: datetime.datetime | Unset = UNSET
    """Timestamp of the most recent probe. Omitted when no probe yet."""
    additional_properties: dict[str, Any] = _attrs_field(init=False, factory=dict)

    def to_dict(self) -> dict[str, Any]:
        id = str(self.id)

        source: str = self.source

        kind: str = self.kind

        host_redacted_hash = self.host_redacted_hash

        port = self.port

        created_at = self.created_at.isoformat()

        last_seen_at = self.last_seen_at.isoformat()

        host_last4 = self.host_last4

        scope = self.scope

        declared_region = self.declared_region

        last_rtt_ms = self.last_rtt_ms

        last_probed_at: str | Unset = UNSET
        if not isinstance(self.last_probed_at, Unset):
            last_probed_at = self.last_probed_at.isoformat()

        field_dict: dict[str, Any] = {}
        field_dict.update(self.additional_properties)
        field_dict.update(
            {
                "id": id,
                "source": source,
                "kind": kind,
                "host_redacted_hash": host_redacted_hash,
                "port": port,
                "created_at": created_at,
                "last_seen_at": last_seen_at,
            }
        )
        if host_last4 is not UNSET:
            field_dict["host_last4"] = host_last4
        if scope is not UNSET:
            field_dict["scope"] = scope
        if declared_region is not UNSET:
            field_dict["declared_region"] = declared_region
        if last_rtt_ms is not UNSET:
            field_dict["last_rtt_ms"] = last_rtt_ms
        if last_probed_at is not UNSET:
            field_dict["last_probed_at"] = last_probed_at

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        d = dict(src_dict)
        id = UUID(d.pop("id"))

        source = check_data_upstream_response_source(d.pop("source"))

        kind = check_data_upstream_response_kind(d.pop("kind"))

        host_redacted_hash = d.pop("host_redacted_hash")

        port = d.pop("port")

        created_at = datetime.datetime.fromisoformat(d.pop("created_at"))

        last_seen_at = datetime.datetime.fromisoformat(d.pop("last_seen_at"))

        host_last4 = d.pop("host_last4", UNSET)

        scope = d.pop("scope", UNSET)

        declared_region = d.pop("declared_region", UNSET)

        last_rtt_ms = d.pop("last_rtt_ms", UNSET)

        _last_probed_at = d.pop("last_probed_at", UNSET)
        last_probed_at: datetime.datetime | Unset
        if isinstance(_last_probed_at, Unset):
            last_probed_at = UNSET
        else:
            last_probed_at = datetime.datetime.fromisoformat(_last_probed_at)

        data_upstream_response = cls(
            id=id,
            source=source,
            kind=kind,
            host_redacted_hash=host_redacted_hash,
            port=port,
            created_at=created_at,
            last_seen_at=last_seen_at,
            host_last4=host_last4,
            scope=scope,
            declared_region=declared_region,
            last_rtt_ms=last_rtt_ms,
            last_probed_at=last_probed_at,
        )

        data_upstream_response.additional_properties = d
        return data_upstream_response

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
