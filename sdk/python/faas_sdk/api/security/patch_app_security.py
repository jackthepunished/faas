from http import HTTPStatus
from typing import Any
from urllib.parse import quote

import httpx

from ... import errors
from ...client import AuthenticatedClient, Client
from ...models.app_security_request import AppSecurityRequest
from ...models.app_security_response import AppSecurityResponse
from ...models.problem import Problem
from ...types import Response


def _get_kwargs(
    slug: str,
    *,
    body: AppSecurityRequest,
) -> dict[str, Any]:
    headers: dict[str, Any] = {}

    _kwargs: dict[str, Any] = {
        "method": "patch",
        "url": "/v1/apps/{slug}/security".format(
            slug=quote(str(slug), safe=""),
        ),
    }

    _kwargs["json"] = body.to_dict()

    headers["Content-Type"] = "application/json"

    _kwargs["headers"] = headers
    return _kwargs


def _parse_response(
    *, client: AuthenticatedClient | Client, response: httpx.Response
) -> AppSecurityResponse | Problem | None:
    if response.status_code == 200:
        response_200 = AppSecurityResponse.from_dict(response.json())

        return response_200

    if response.status_code == 400:
        response_400 = Problem.from_dict(response.json())

        return response_400

    if response.status_code == 401:
        response_401 = Problem.from_dict(response.json())

        return response_401

    if response.status_code == 403:
        response_403 = Problem.from_dict(response.json())

        return response_403

    if response.status_code == 404:
        response_404 = Problem.from_dict(response.json())

        return response_404

    if response.status_code == 500:
        response_500 = Problem.from_dict(response.json())

        return response_500

    if client.raise_on_unexpected_status:
        raise errors.UnexpectedStatus(response.status_code, response.content)
    else:
        return None


def _build_response(
    *, client: AuthenticatedClient | Client, response: httpx.Response
) -> Response[AppSecurityResponse | Problem]:
    return Response(
        status_code=HTTPStatus(response.status_code),
        content=response.content,
        headers=response.headers,
        parsed=_parse_response(client=client, response=response),
    )


def sync_detailed(
    slug: str,
    *,
    client: AuthenticatedClient | Client,
    body: AppSecurityRequest,
) -> Response[AppSecurityResponse | Problem]:
    """Toggle the require_signed flag for an app (admin + MFA).

     Operator-only surface for the per-app cosign signature-enforcement
    flag (issue #472 / ADR-054). Mounted with `authLimited → requireMFA →
    requireScope(ScopesAdminOnly...)`. The customer PATCH /v1/apps/{slug}
    endpoint silently drops the field — flipping it through that surface
    is a no-op — so the only path that persists `require_signed=true` is
    this one.

    `nil` = no field set (no-op 200). Non-nil = atomic overwrite.

    Audit event: `app.security_updated` carries old/new values.

    Args:
        slug (str):
        body (AppSecurityRequest): PATCH body for `/v1/apps/{slug}/security`. `require_signed` is
            a
            pointer so the wire form can distinguish "don't touch" (nil) from
            "explicit true/false" — the same Set-bit convention the broader
            UpdateAppRequest uses (issue #471 streaming flag precedent).

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        Response[AppSecurityResponse | Problem]
    """

    kwargs = _get_kwargs(
        slug=slug,
        body=body,
    )

    response = client.get_httpx_client().request(
        **kwargs,
    )

    return _build_response(client=client, response=response)


def sync(
    slug: str,
    *,
    client: AuthenticatedClient | Client,
    body: AppSecurityRequest,
) -> AppSecurityResponse | Problem | None:
    """Toggle the require_signed flag for an app (admin + MFA).

     Operator-only surface for the per-app cosign signature-enforcement
    flag (issue #472 / ADR-054). Mounted with `authLimited → requireMFA →
    requireScope(ScopesAdminOnly...)`. The customer PATCH /v1/apps/{slug}
    endpoint silently drops the field — flipping it through that surface
    is a no-op — so the only path that persists `require_signed=true` is
    this one.

    `nil` = no field set (no-op 200). Non-nil = atomic overwrite.

    Audit event: `app.security_updated` carries old/new values.

    Args:
        slug (str):
        body (AppSecurityRequest): PATCH body for `/v1/apps/{slug}/security`. `require_signed` is
            a
            pointer so the wire form can distinguish "don't touch" (nil) from
            "explicit true/false" — the same Set-bit convention the broader
            UpdateAppRequest uses (issue #471 streaming flag precedent).

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        AppSecurityResponse | Problem
    """

    return sync_detailed(
        slug=slug,
        client=client,
        body=body,
    ).parsed


async def asyncio_detailed(
    slug: str,
    *,
    client: AuthenticatedClient | Client,
    body: AppSecurityRequest,
) -> Response[AppSecurityResponse | Problem]:
    """Toggle the require_signed flag for an app (admin + MFA).

     Operator-only surface for the per-app cosign signature-enforcement
    flag (issue #472 / ADR-054). Mounted with `authLimited → requireMFA →
    requireScope(ScopesAdminOnly...)`. The customer PATCH /v1/apps/{slug}
    endpoint silently drops the field — flipping it through that surface
    is a no-op — so the only path that persists `require_signed=true` is
    this one.

    `nil` = no field set (no-op 200). Non-nil = atomic overwrite.

    Audit event: `app.security_updated` carries old/new values.

    Args:
        slug (str):
        body (AppSecurityRequest): PATCH body for `/v1/apps/{slug}/security`. `require_signed` is
            a
            pointer so the wire form can distinguish "don't touch" (nil) from
            "explicit true/false" — the same Set-bit convention the broader
            UpdateAppRequest uses (issue #471 streaming flag precedent).

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        Response[AppSecurityResponse | Problem]
    """

    kwargs = _get_kwargs(
        slug=slug,
        body=body,
    )

    response = await client.get_async_httpx_client().request(**kwargs)

    return _build_response(client=client, response=response)


async def asyncio(
    slug: str,
    *,
    client: AuthenticatedClient | Client,
    body: AppSecurityRequest,
) -> AppSecurityResponse | Problem | None:
    """Toggle the require_signed flag for an app (admin + MFA).

     Operator-only surface for the per-app cosign signature-enforcement
    flag (issue #472 / ADR-054). Mounted with `authLimited → requireMFA →
    requireScope(ScopesAdminOnly...)`. The customer PATCH /v1/apps/{slug}
    endpoint silently drops the field — flipping it through that surface
    is a no-op — so the only path that persists `require_signed=true` is
    this one.

    `nil` = no field set (no-op 200). Non-nil = atomic overwrite.

    Audit event: `app.security_updated` carries old/new values.

    Args:
        slug (str):
        body (AppSecurityRequest): PATCH body for `/v1/apps/{slug}/security`. `require_signed` is
            a
            pointer so the wire form can distinguish "don't touch" (nil) from
            "explicit true/false" — the same Set-bit convention the broader
            UpdateAppRequest uses (issue #471 streaming flag precedent).

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        AppSecurityResponse | Problem
    """

    return (
        await asyncio_detailed(
            slug=slug,
            client=client,
            body=body,
        )
    ).parsed
