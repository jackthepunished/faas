from __future__ import annotations

from collections.abc import Mapping
from typing import TYPE_CHECKING, Any, TypeVar, cast

from attrs import define as _attrs_define
from attrs import field as _attrs_field

from ..types import UNSET, Unset

if TYPE_CHECKING:
    from ..models.create_deployment_overrides import CreateDeploymentOverrides
    from ..models.sidecar import Sidecar


T = TypeVar("T", bound="CreateDeploymentRequest")


@_attrs_define
class CreateDeploymentRequest:
    """Two content-types accepted (see operation description): prebuilt OCI image reference, or multipart source upload.
    The optional `overrides` object (issue #460 / ADR-053) lets a customer redeploy the same digest-pinned image with a
    different entrypoint / cmd / env / env_secrets / port / healthcheck without rebuilding the image. The override field
    list is FROZEN — six fields, no more — and any extra field on the override object 400s the request (the handler's
    decoder rejects unknown keys; see ADR-053 §Decision 1). The optional `sidecars` array (issue #463 / ADR-068)
    attaches up to 2 stateless sidecars (1 init + 1 sidecar) per app — a one-shot DB migrator as `init`, a metrics
    scraper as `sidecar`. nil/omitted = no sidecars.

    """

    image: str | Unset = UNSET
    """registry.gregale.dev/...@sha256:... — digest-pinned OCI reference."""
    overrides: CreateDeploymentOverrides | None | Unset = UNSET
    """Deploy-time overrides (entrypoint, cmd, env, env_secrets, port, healthcheck). nil/omitted = deploy the image
    as-is."""
    require_signed: bool | None | Unset = UNSET
    """Per-deploy signature-enforcement opt-in (issue #472 / ADR-054). nil = inherit apps.require_signed; *true is
    a no-op when the app flag is already on; *false is rejected with 403 deploy_signature_invalid when the app flag
    is on (operator policy wins)."""
    sidecars: list[Sidecar] | Unset = UNSET
    """Up to 2 stateless sidecars (1 init + 1 sidecar). nil/omitted = no sidecars. See ADR-068 for the hard 2-cap
    and stateless-only contract."""
    traffic_percent: int | None | Unset = UNSET
    """Per-deployment traffic-split weight (issue #556 PR-A). nil = server default 100; explicit 0..100 = opt into
    canary (Pro/Scale only)."""
    scope: None | str | Unset = UNSET
    """Top-level per-deployment env scope (ADR-091 / PR-D). Lowercase alnum + dash, 3..40 chars, no
    leading/trailing dash. nil/omitted = `default`."""
    additional_properties: dict[str, Any] = _attrs_field(init=False, factory=dict)

    def to_dict(self) -> dict[str, Any]:
        from ..models.create_deployment_overrides import CreateDeploymentOverrides

        image = self.image

        overrides: dict[str, Any] | None | Unset
        if isinstance(self.overrides, Unset):
            overrides = UNSET
        elif isinstance(self.overrides, CreateDeploymentOverrides):
            overrides = self.overrides.to_dict()
        else:
            overrides = self.overrides

        require_signed: bool | None | Unset
        if isinstance(self.require_signed, Unset):
            require_signed = UNSET
        else:
            require_signed = self.require_signed

        sidecars: list[dict[str, Any]] | Unset = UNSET
        if not isinstance(self.sidecars, Unset):
            sidecars = []
            for sidecars_item_data in self.sidecars:
                sidecars_item = sidecars_item_data.to_dict()
                sidecars.append(sidecars_item)

        traffic_percent: int | None | Unset
        if isinstance(self.traffic_percent, Unset):
            traffic_percent = UNSET
        else:
            traffic_percent = self.traffic_percent

        scope: None | str | Unset
        if isinstance(self.scope, Unset):
            scope = UNSET
        else:
            scope = self.scope

        field_dict: dict[str, Any] = {}
        field_dict.update(self.additional_properties)
        field_dict.update({})
        if image is not UNSET:
            field_dict["image"] = image
        if overrides is not UNSET:
            field_dict["overrides"] = overrides
        if require_signed is not UNSET:
            field_dict["require_signed"] = require_signed
        if sidecars is not UNSET:
            field_dict["sidecars"] = sidecars
        if traffic_percent is not UNSET:
            field_dict["traffic_percent"] = traffic_percent
        if scope is not UNSET:
            field_dict["scope"] = scope

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        from ..models.create_deployment_overrides import CreateDeploymentOverrides
        from ..models.sidecar import Sidecar

        d = dict(src_dict)
        image = d.pop("image", UNSET)

        def _parse_overrides(data: object) -> CreateDeploymentOverrides | None | Unset:
            if data is None:
                return data
            if isinstance(data, Unset):
                return data
            try:
                if not isinstance(data, dict):
                    raise TypeError()
                overrides_type_0 = CreateDeploymentOverrides.from_dict(data)

                return overrides_type_0
            except (TypeError, ValueError, AttributeError, KeyError):
                pass
            return cast(CreateDeploymentOverrides | None | Unset, data)

        overrides = _parse_overrides(d.pop("overrides", UNSET))

        def _parse_require_signed(data: object) -> bool | None | Unset:
            if data is None:
                return data
            if isinstance(data, Unset):
                return data
            return cast(bool | None | Unset, data)

        require_signed = _parse_require_signed(d.pop("require_signed", UNSET))

        _sidecars = d.pop("sidecars", UNSET)
        sidecars: list[Sidecar] | Unset = UNSET
        if _sidecars is not UNSET:
            sidecars = []
            for sidecars_item_data in _sidecars:
                sidecars_item = Sidecar.from_dict(sidecars_item_data)

                sidecars.append(sidecars_item)

        def _parse_traffic_percent(data: object) -> int | None | Unset:
            if data is None:
                return data
            if isinstance(data, Unset):
                return data
            return cast(int | None | Unset, data)

        traffic_percent = _parse_traffic_percent(d.pop("traffic_percent", UNSET))

        def _parse_scope(data: object) -> None | str | Unset:
            if data is None:
                return data
            if isinstance(data, Unset):
                return data
            return cast(None | str | Unset, data)

        scope = _parse_scope(d.pop("scope", UNSET))

        create_deployment_request = cls(
            image=image,
            overrides=overrides,
            require_signed=require_signed,
            sidecars=sidecars,
            traffic_percent=traffic_percent,
            scope=scope,
        )

        create_deployment_request.additional_properties = d
        return create_deployment_request

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
