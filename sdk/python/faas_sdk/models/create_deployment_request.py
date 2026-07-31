from __future__ import annotations

from collections.abc import Mapping
from typing import TYPE_CHECKING, Any, TypeVar, cast

from attrs import define as _attrs_define
from attrs import field as _attrs_field

from ..types import UNSET, Unset

if TYPE_CHECKING:
    from ..models.create_deployment_overrides import CreateDeploymentOverrides


T = TypeVar("T", bound="CreateDeploymentRequest")


@_attrs_define
class CreateDeploymentRequest:
    """Two content-types accepted (see operation description): prebuilt OCI image reference, or multipart source upload.
    The optional `overrides` object (issue #460 / ADR-053) lets a customer redeploy the same digest-pinned image with a
    different entrypoint / cmd / env / env_secrets / port / healthcheck without rebuilding the image. The override field
    list is FROZEN — six fields, no more — and any extra field on the override object 400s the request (the handler's
    decoder rejects unknown keys; see ADR-053 §Decision 1).

    """

    image: str | Unset = UNSET
    """registry.gregale.dev/...@sha256:... — digest-pinned OCI reference."""
    overrides: CreateDeploymentOverrides | None | Unset = UNSET
    """Deploy-time overrides (entrypoint, cmd, env, env_secrets, port, healthcheck). nil/omitted = deploy the image
    as-is."""
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

        field_dict: dict[str, Any] = {}
        field_dict.update(self.additional_properties)
        field_dict.update({})
        if image is not UNSET:
            field_dict["image"] = image
        if overrides is not UNSET:
            field_dict["overrides"] = overrides

        return field_dict

    @classmethod
    def from_dict(cls: type[T], src_dict: Mapping[str, Any]) -> T:
        from ..models.create_deployment_overrides import CreateDeploymentOverrides

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

        create_deployment_request = cls(
            image=image,
            overrides=overrides,
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
