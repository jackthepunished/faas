from __future__ import annotations

import datetime
from collections.abc import Mapping
from typing import TYPE_CHECKING, Any, TypeVar, cast

from attrs import define as _attrs_define
from attrs import field as _attrs_field

from ..models.scan_result_status import ScanResultStatus, check_scan_result_status
from ..types import UNSET, Unset

if TYPE_CHECKING:
    from ..models.severity_counts import SeverityCounts
    from ..models.vulnerability import Vulnerability


T = TypeVar("T", bound="ScanResult")


@_attrs_define
class ScanResult:
    """Per-deploy grype CVE scan result (issue #464 / ADR-055). Surfaced on
    GET /v1/deployments/{id} (additive DeploymentResponse.Scan field) and
    on GET /v1/deployments/{id}/scan (the dedicated drill-down route).
    The dashboard renders the severity counts and the top 10 CVEs;
    `gregale deployment <id> --show-scan` prints the full payload.
    Surface, never enforce — an image with CRITICAL CVEs deploys
    successfully; the dashboard shows it; that is the contract.

    """

    status: ScanResultStatus
    """Closed enum mirroring the deployments.scan_status column.
    `pending` = grype run started, not finished yet;
    `complete` = grype run finished, scan carries the findings;
    `failed` = grype run errored after the 1-retry backoff, scan carries the last error in `error`;
    `skipped` = pre-feature row (the migration backfilled this on every row that predates 00135).
    """
    severity_counts: SeverityCounts
    """Per-bucket count of CVEs in Grype's closed vocabulary
    (CRITICAL|HIGH|MEDIUM|LOW|UNKNOWN). Negligible collapses into LOW
    (matches the existing pkg/imaged.grype.go::normalizeGrypeSeverity
    convention). All fields present without omitempty so the JSON shape
    is uniform — the dashboard reads counts without nil checks.
    """
    vulnerabilities: list[Vulnerability]
    """Full CVE list, ordered by Grype's natural output (most-severe-first). The dashboard's "top 10" view
    sorts+truncates client-side. The /scan route returns the full list."""
    scanned_at: datetime.datetime | None | Unset = UNSET
    """Wall clock the grype run completed (RFC 3339 UTC). Empty when status != "complete". Distinct from
    deployments.created_at — the deploy ships before the scan lands (AC"""
    scanner_version: None | str | Unset = UNSET
    """Grype binary version that produced the scan (e.g. "grype 0.78.0"). Captured once at imaged startup via
    `grype version` and stamped on every ScanResult payload."""
    image_digest: None | str | Unset = UNSET
    """OCI image digest at the time of the scan. Sourced from deployments.image_digest, not re-inspected. Empty on
    the pre-feature backfill (status = "skipped" with no image to stamp)."""
    error: None | str | Unset = UNSET
    """Grype runner's last error message on a failed scan (status = "failed"). Empty on every other status. The
    PR-3 sink captures the message after the 1-retry backoff is exhausted."""
    additional_properties: dict[str, Any] = _attrs_field(init=False, factory=dict)

    def to_dict(self) -> dict[str, Any]:
        status: str = self.status

        severity_counts = self.severity_counts.to_dict()

        vulnerabilities = []
        for vulnerabilities_item_data in self.vulnerabilities:
            vulnerabilities_item = vulnerabilities_item_data.to_dict()
            vulnerabilities.append(vulnerabilities_item)

        scanned_at: None | str | Unset
        if isinstance(self.scanned_at, Unset):
            scanned_at = UNSET
        elif isinstance(self.scanned_at, datetime.datetime):
            scanned_at = self.scanned_at.isoformat()
        else:
            scanned_at = self.scanned_at

        scanner_version: None | str | Unset
        if isinstance(self.scanner_version, Unset):
            scanner_version = UNSET
        else:
            scanner_version = self.scanner_version

        image_digest: None | str | Unset
        if isinstance(self.image_digest, Unset):
            image_digest = UNSET
        else:
            image_digest = self.image_digest

        error: None | str | Unset
        if isinstance(self.error, Unset):
            error = UNSET
        else:
            error = self.error

        field_dict: dict[str, Any] = {}
        field_dict.update(self.additional_properties)
        field_dict.update(
            {
                "status": status,
                "severity_counts": severity_counts,
                "vulnerabilities": vulnerabilities,
            }
        )
        if scanned_at is not UNSET:
            field_dict["scanned_at"] = scanned_at
        if scanner_version is not UNSET:
            field_dict["scanner_version"] = scanner_version
        if image_digest is not UNSET:
            field_dict["image_digest"] = image_digest
        if error is not UNSET:
            field_dict["error"] = error

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        from ..models.severity_counts import SeverityCounts
        from ..models.vulnerability import Vulnerability

        d = dict(src_dict)
        status = check_scan_result_status(d.pop("status"))

        severity_counts = SeverityCounts.from_dict(d.pop("severity_counts"))

        vulnerabilities = []
        _vulnerabilities = d.pop("vulnerabilities")
        for vulnerabilities_item_data in _vulnerabilities:
            vulnerabilities_item = Vulnerability.from_dict(vulnerabilities_item_data)

            vulnerabilities.append(vulnerabilities_item)

        def _parse_scanned_at(data: object) -> datetime.datetime | None | Unset:
            if data is None:
                return data
            if isinstance(data, Unset):
                return data
            try:
                if not isinstance(data, str):
                    raise TypeError()
                scanned_at_type_0 = datetime.datetime.fromisoformat(data)

                return scanned_at_type_0
            except (TypeError, ValueError, AttributeError, KeyError):
                pass
            return cast(datetime.datetime | None | Unset, data)

        scanned_at = _parse_scanned_at(d.pop("scanned_at", UNSET))

        def _parse_scanner_version(data: object) -> None | str | Unset:
            if data is None:
                return data
            if isinstance(data, Unset):
                return data
            return cast(None | str | Unset, data)

        scanner_version = _parse_scanner_version(d.pop("scanner_version", UNSET))

        def _parse_image_digest(data: object) -> None | str | Unset:
            if data is None:
                return data
            if isinstance(data, Unset):
                return data
            return cast(None | str | Unset, data)

        image_digest = _parse_image_digest(d.pop("image_digest", UNSET))

        def _parse_error(data: object) -> None | str | Unset:
            if data is None:
                return data
            if isinstance(data, Unset):
                return data
            return cast(None | str | Unset, data)

        error = _parse_error(d.pop("error", UNSET))

        scan_result = cls(
            status=status,
            severity_counts=severity_counts,
            vulnerabilities=vulnerabilities,
            scanned_at=scanned_at,
            scanner_version=scanner_version,
            image_digest=image_digest,
            error=error,
        )

        scan_result.additional_properties = d
        return scan_result

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
