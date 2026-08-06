from __future__ import annotations

import datetime
from collections.abc import Mapping
from typing import TYPE_CHECKING, Any, TypeVar, cast

from attrs import define as _attrs_define
from attrs import field as _attrs_field

from ..types import UNSET, Unset

if TYPE_CHECKING:
    from ..models.deployment_healthcheck import DeploymentHealthcheck
    from ..models.deployment_liveness_probe import DeploymentLivenessProbe
    from ..models.deployment_response_override_env_secret_refs import DeploymentResponseOverrideEnvSecretRefs


T = TypeVar("T", bound="DeploymentResponse")


@_attrs_define
class DeploymentResponse:
    """One deployment: id, app, source ref, build status, commit SHA, and lifecycle timestamps. The optional
    `has_overrides` and `override_*` fields are the persisted echo of the create-time overrides object (issue #460 /
    ADR-053); they round-trip via `GET /v1/apps/{slug}/deployments/{id}` so a customer can audit what their last deploy
    pinned. Env values are NEVER echoed — only the keys (`override_env_keys`); env_secrets refs ARE echoed because the
    ref shape is non-secret by design.

    """

    id: str
    app_id: str
    image_digest: str
    kind: str
    status: str
    created_at: datetime.datetime
    build_id: None | str | Unset = UNSET
    error: None | str | Unset = UNSET
    error_code: None | str | Unset = UNSET
    has_overrides: bool | Unset = UNSET
    """True when this deployment carries a non-null override_* column set."""
    override_entrypoint: list[str] | Unset = UNSET
    """Entrypoint override echoed verbatim from the create request. nil when no override was supplied."""
    override_cmd: list[str] | Unset = UNSET
    """Cmd override echoed verbatim from the create request."""
    override_env_keys: list[str] | Unset = UNSET
    """Sorted set of env-var keys set by the env override. VALUES ARE NEVER ECHOED (ADR-053 §Decision 4)."""
    override_env_secret_keys: list[str] | Unset = UNSET
    """Sorted set of env-var keys set by the env_secrets override. The parallel refs are echoed in
    `override_env_secret_refs` because the ref shape is non-secret by design."""
    override_env_secret_refs: DeploymentResponseOverrideEnvSecretRefs | Unset = UNSET
    """Verbatim `secret:NAME` ref map; the customer needs to see which secret they bound to which env var to debug
    a misconfigured deploy."""
    override_port: int | Unset = UNSET
    """Listen-port override; 0 = absent (fall back to image default)."""
    override_healthcheck: DeploymentHealthcheck | None | Unset = UNSET
    """Readiness-probe override. Persisted verbatim; the actual HTTP probe is a follow-up — today waitReady stays a
    bare TCP accept."""
    override_liveness_probe: DeploymentLivenessProbe | None | Unset = UNSET
    """Liveness-probe override echoed verbatim (issue #554 / ADR-078). nil when the deployment used the per-plan
    default (Hobby/Pro/Scale → 5s / 3 consecutive / 60s cooldown). Echoed on GET /v1/apps/{slug}/deployments/{id} so
    the customer can audit which probe the host (cmd/vmmd) is running against the VM."""
    min_instances: int | Unset = UNSET
    """Per-deployment cold-wake floor override (issue #557 closure / ADR-072). 0 = inherit from parent app
    (default); positive value is the deployment's own floor. Effective per-instance floor =
    max(app.EffectiveMinInstances(), d.EffectiveMinInstances()). Validated against the parent app's plan
    MaxMinInstances cap on PATCH."""
    additional_properties: dict[str, Any] = _attrs_field(init=False, factory=dict)

    def to_dict(self) -> dict[str, Any]:
        from ..models.deployment_healthcheck import DeploymentHealthcheck
        from ..models.deployment_liveness_probe import DeploymentLivenessProbe

        id = self.id

        app_id = self.app_id

        image_digest = self.image_digest

        kind = self.kind

        status = self.status

        created_at = self.created_at.isoformat()

        build_id: None | str | Unset
        if isinstance(self.build_id, Unset):
            build_id = UNSET
        else:
            build_id = self.build_id

        error: None | str | Unset
        if isinstance(self.error, Unset):
            error = UNSET
        else:
            error = self.error

        error_code: None | str | Unset
        if isinstance(self.error_code, Unset):
            error_code = UNSET
        else:
            error_code = self.error_code

        has_overrides = self.has_overrides

        override_entrypoint: list[str] | Unset = UNSET
        if not isinstance(self.override_entrypoint, Unset):
            override_entrypoint = self.override_entrypoint

        override_cmd: list[str] | Unset = UNSET
        if not isinstance(self.override_cmd, Unset):
            override_cmd = self.override_cmd

        override_env_keys: list[str] | Unset = UNSET
        if not isinstance(self.override_env_keys, Unset):
            override_env_keys = self.override_env_keys

        override_env_secret_keys: list[str] | Unset = UNSET
        if not isinstance(self.override_env_secret_keys, Unset):
            override_env_secret_keys = self.override_env_secret_keys

        override_env_secret_refs: dict[str, Any] | Unset = UNSET
        if not isinstance(self.override_env_secret_refs, Unset):
            override_env_secret_refs = self.override_env_secret_refs.to_dict()

        override_port = self.override_port

        override_healthcheck: dict[str, Any] | None | Unset
        if isinstance(self.override_healthcheck, Unset):
            override_healthcheck = UNSET
        elif isinstance(self.override_healthcheck, DeploymentHealthcheck):
            override_healthcheck = self.override_healthcheck.to_dict()
        else:
            override_healthcheck = self.override_healthcheck

        override_liveness_probe: dict[str, Any] | None | Unset
        if isinstance(self.override_liveness_probe, Unset):
            override_liveness_probe = UNSET
        elif isinstance(self.override_liveness_probe, DeploymentLivenessProbe):
            override_liveness_probe = self.override_liveness_probe.to_dict()
        else:
            override_liveness_probe = self.override_liveness_probe

        min_instances = self.min_instances

        field_dict: dict[str, Any] = {}
        field_dict.update(self.additional_properties)
        field_dict.update(
            {
                "id": id,
                "app_id": app_id,
                "image_digest": image_digest,
                "kind": kind,
                "status": status,
                "created_at": created_at,
            }
        )
        if build_id is not UNSET:
            field_dict["build_id"] = build_id
        if error is not UNSET:
            field_dict["error"] = error
        if error_code is not UNSET:
            field_dict["error_code"] = error_code
        if has_overrides is not UNSET:
            field_dict["has_overrides"] = has_overrides
        if override_entrypoint is not UNSET:
            field_dict["override_entrypoint"] = override_entrypoint
        if override_cmd is not UNSET:
            field_dict["override_cmd"] = override_cmd
        if override_env_keys is not UNSET:
            field_dict["override_env_keys"] = override_env_keys
        if override_env_secret_keys is not UNSET:
            field_dict["override_env_secret_keys"] = override_env_secret_keys
        if override_env_secret_refs is not UNSET:
            field_dict["override_env_secret_refs"] = override_env_secret_refs
        if override_port is not UNSET:
            field_dict["override_port"] = override_port
        if override_healthcheck is not UNSET:
            field_dict["override_healthcheck"] = override_healthcheck
        if override_liveness_probe is not UNSET:
            field_dict["override_liveness_probe"] = override_liveness_probe
        if min_instances is not UNSET:
            field_dict["min_instances"] = min_instances

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        from ..models.deployment_healthcheck import DeploymentHealthcheck
        from ..models.deployment_liveness_probe import DeploymentLivenessProbe
        from ..models.deployment_response_override_env_secret_refs import DeploymentResponseOverrideEnvSecretRefs

        d = dict(src_dict)
        id = d.pop("id")

        app_id = d.pop("app_id")

        image_digest = d.pop("image_digest")

        kind = d.pop("kind")

        status = d.pop("status")

        created_at = datetime.datetime.fromisoformat(d.pop("created_at"))

        def _parse_build_id(data: object) -> None | str | Unset:
            if data is None:
                return data
            if isinstance(data, Unset):
                return data
            return cast(None | str | Unset, data)

        build_id = _parse_build_id(d.pop("build_id", UNSET))

        def _parse_error(data: object) -> None | str | Unset:
            if data is None:
                return data
            if isinstance(data, Unset):
                return data
            return cast(None | str | Unset, data)

        error = _parse_error(d.pop("error", UNSET))

        def _parse_error_code(data: object) -> None | str | Unset:
            if data is None:
                return data
            if isinstance(data, Unset):
                return data
            return cast(None | str | Unset, data)

        error_code = _parse_error_code(d.pop("error_code", UNSET))

        has_overrides = d.pop("has_overrides", UNSET)

        override_entrypoint = cast(list[str], d.pop("override_entrypoint", UNSET))

        override_cmd = cast(list[str], d.pop("override_cmd", UNSET))

        override_env_keys = cast(list[str], d.pop("override_env_keys", UNSET))

        override_env_secret_keys = cast(list[str], d.pop("override_env_secret_keys", UNSET))

        _override_env_secret_refs = d.pop("override_env_secret_refs", UNSET)
        override_env_secret_refs: DeploymentResponseOverrideEnvSecretRefs | Unset
        if isinstance(_override_env_secret_refs, Unset):
            override_env_secret_refs = UNSET
        else:
            override_env_secret_refs = DeploymentResponseOverrideEnvSecretRefs.from_dict(_override_env_secret_refs)

        override_port = d.pop("override_port", UNSET)

        def _parse_override_healthcheck(data: object) -> DeploymentHealthcheck | None | Unset:
            if data is None:
                return data
            if isinstance(data, Unset):
                return data
            try:
                if not isinstance(data, dict):
                    raise TypeError()
                override_healthcheck_type_0 = DeploymentHealthcheck.from_dict(data)

                return override_healthcheck_type_0
            except (TypeError, ValueError, AttributeError, KeyError):
                pass
            return cast(DeploymentHealthcheck | None | Unset, data)

        override_healthcheck = _parse_override_healthcheck(d.pop("override_healthcheck", UNSET))

        def _parse_override_liveness_probe(data: object) -> DeploymentLivenessProbe | None | Unset:
            if data is None:
                return data
            if isinstance(data, Unset):
                return data
            try:
                if not isinstance(data, dict):
                    raise TypeError()
                override_liveness_probe_type_0 = DeploymentLivenessProbe.from_dict(data)

                return override_liveness_probe_type_0
            except (TypeError, ValueError, AttributeError, KeyError):
                pass
            return cast(DeploymentLivenessProbe | None | Unset, data)

        override_liveness_probe = _parse_override_liveness_probe(d.pop("override_liveness_probe", UNSET))

        min_instances = d.pop("min_instances", UNSET)

        deployment_response = cls(
            id=id,
            app_id=app_id,
            image_digest=image_digest,
            kind=kind,
            status=status,
            created_at=created_at,
            build_id=build_id,
            error=error,
            error_code=error_code,
            has_overrides=has_overrides,
            override_entrypoint=override_entrypoint,
            override_cmd=override_cmd,
            override_env_keys=override_env_keys,
            override_env_secret_keys=override_env_secret_keys,
            override_env_secret_refs=override_env_secret_refs,
            override_port=override_port,
            override_healthcheck=override_healthcheck,
            override_liveness_probe=override_liveness_probe,
            min_instances=min_instances,
        )

        deployment_response.additional_properties = d
        return deployment_response

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
