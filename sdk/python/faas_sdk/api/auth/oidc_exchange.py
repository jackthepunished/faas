from http import HTTPStatus
from typing import Any

import httpx

from ... import errors
from ...client import AuthenticatedClient, Client
from ...models.oidc_exchange_request import OIDCExchangeRequest
from ...models.oidc_exchange_response import OIDCExchangeResponse
from ...models.problem import Problem
from ...types import Response


def _get_kwargs(
    *,
    body: OIDCExchangeRequest,
) -> dict[str, Any]:
    headers: dict[str, Any] = {}

    _kwargs: dict[str, Any] = {
        "method": "post",
        "url": "/v1/auth/oidc/exchange",
    }

    _kwargs["json"] = body.to_dict()

    headers["Content-Type"] = "application/json"

    _kwargs["headers"] = headers
    return _kwargs


def _parse_response(
    *, client: AuthenticatedClient | Client, response: httpx.Response
) -> OIDCExchangeResponse | Problem | None:
    if response.status_code == 200:
        response_200 = OIDCExchangeResponse.from_dict(response.json())

        return response_200

    if response.status_code == 400:
        response_400 = Problem.from_dict(response.json())

        return response_400

    if response.status_code == 401:
        response_401 = Problem.from_dict(response.json())

        return response_401

    if response.status_code == 429:
        response_429 = Problem.from_dict(response.json())

        return response_429

    if client.raise_on_unexpected_status:
        raise errors.UnexpectedStatus(response.status_code, response.content)
    else:
        return None


def _build_response(
    *, client: AuthenticatedClient | Client, response: httpx.Response
) -> Response[OIDCExchangeResponse | Problem]:
    return Response(
        status_code=HTTPStatus(response.status_code),
        content=response.content,
        headers=response.headers,
        parsed=_parse_response(client=client, response=response),
    )


def sync_detailed(
    *,
    client: AuthenticatedClient | Client,
    body: OIDCExchangeRequest,
) -> Response[OIDCExchangeResponse | Problem]:
    """Exchange an IdP-issued JWT for a short-lived deploy bearer

     ADR-101 / issue #270. CI runners that have an IdP-issued OIDC
    JWT (RFC 8414; e.g. GitHub Actions `ACTIONS_ID_TOKEN_REQUEST_TOKEN`,
    GitLab CI, CircleCI) call this endpoint to exchange it for a
    short-lived opaque bearer (5 min TTL, `fp_oidc_<48 hex>` prefix).
    The bearer is then used in `Authorization: Bearer …` on the
    existing deploy routes.

    The endpoint is anonymous — the JWT is the auth — so it does
    not require a session or a previous bearer. The first-use
    auto-create flow bootstraps a permissive trust policy on the
    `(account_id, issuer_url)` pair so customers do not have to
    configure the dashboard before their first CI deploy.

    The AuthLimit surface is the shared per-IP bucket (spec §11
    10/min/IP) — high-volume CI runners may hit the cap; long-lived
    deploy tokens remain the escape hatch.

    Args:
        body (OIDCExchangeRequest): Body for `POST /v1/auth/oidc/exchange` (ADR-101).

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        Response[OIDCExchangeResponse | Problem]
    """

    kwargs = _get_kwargs(
        body=body,
    )

    response = client.get_httpx_client().request(
        **kwargs,
    )

    return _build_response(client=client, response=response)


def sync(
    *,
    client: AuthenticatedClient | Client,
    body: OIDCExchangeRequest,
) -> OIDCExchangeResponse | Problem | None:
    """Exchange an IdP-issued JWT for a short-lived deploy bearer

     ADR-101 / issue #270. CI runners that have an IdP-issued OIDC
    JWT (RFC 8414; e.g. GitHub Actions `ACTIONS_ID_TOKEN_REQUEST_TOKEN`,
    GitLab CI, CircleCI) call this endpoint to exchange it for a
    short-lived opaque bearer (5 min TTL, `fp_oidc_<48 hex>` prefix).
    The bearer is then used in `Authorization: Bearer …` on the
    existing deploy routes.

    The endpoint is anonymous — the JWT is the auth — so it does
    not require a session or a previous bearer. The first-use
    auto-create flow bootstraps a permissive trust policy on the
    `(account_id, issuer_url)` pair so customers do not have to
    configure the dashboard before their first CI deploy.

    The AuthLimit surface is the shared per-IP bucket (spec §11
    10/min/IP) — high-volume CI runners may hit the cap; long-lived
    deploy tokens remain the escape hatch.

    Args:
        body (OIDCExchangeRequest): Body for `POST /v1/auth/oidc/exchange` (ADR-101).

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        OIDCExchangeResponse | Problem
    """

    return sync_detailed(
        client=client,
        body=body,
    ).parsed


async def asyncio_detailed(
    *,
    client: AuthenticatedClient | Client,
    body: OIDCExchangeRequest,
) -> Response[OIDCExchangeResponse | Problem]:
    """Exchange an IdP-issued JWT for a short-lived deploy bearer

     ADR-101 / issue #270. CI runners that have an IdP-issued OIDC
    JWT (RFC 8414; e.g. GitHub Actions `ACTIONS_ID_TOKEN_REQUEST_TOKEN`,
    GitLab CI, CircleCI) call this endpoint to exchange it for a
    short-lived opaque bearer (5 min TTL, `fp_oidc_<48 hex>` prefix).
    The bearer is then used in `Authorization: Bearer …` on the
    existing deploy routes.

    The endpoint is anonymous — the JWT is the auth — so it does
    not require a session or a previous bearer. The first-use
    auto-create flow bootstraps a permissive trust policy on the
    `(account_id, issuer_url)` pair so customers do not have to
    configure the dashboard before their first CI deploy.

    The AuthLimit surface is the shared per-IP bucket (spec §11
    10/min/IP) — high-volume CI runners may hit the cap; long-lived
    deploy tokens remain the escape hatch.

    Args:
        body (OIDCExchangeRequest): Body for `POST /v1/auth/oidc/exchange` (ADR-101).

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        Response[OIDCExchangeResponse | Problem]
    """

    kwargs = _get_kwargs(
        body=body,
    )

    response = await client.get_async_httpx_client().request(**kwargs)

    return _build_response(client=client, response=response)


async def asyncio(
    *,
    client: AuthenticatedClient | Client,
    body: OIDCExchangeRequest,
) -> OIDCExchangeResponse | Problem | None:
    """Exchange an IdP-issued JWT for a short-lived deploy bearer

     ADR-101 / issue #270. CI runners that have an IdP-issued OIDC
    JWT (RFC 8414; e.g. GitHub Actions `ACTIONS_ID_TOKEN_REQUEST_TOKEN`,
    GitLab CI, CircleCI) call this endpoint to exchange it for a
    short-lived opaque bearer (5 min TTL, `fp_oidc_<48 hex>` prefix).
    The bearer is then used in `Authorization: Bearer …` on the
    existing deploy routes.

    The endpoint is anonymous — the JWT is the auth — so it does
    not require a session or a previous bearer. The first-use
    auto-create flow bootstraps a permissive trust policy on the
    `(account_id, issuer_url)` pair so customers do not have to
    configure the dashboard before their first CI deploy.

    The AuthLimit surface is the shared per-IP bucket (spec §11
    10/min/IP) — high-volume CI runners may hit the cap; long-lived
    deploy tokens remain the escape hatch.

    Args:
        body (OIDCExchangeRequest): Body for `POST /v1/auth/oidc/exchange` (ADR-101).

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        OIDCExchangeResponse | Problem
    """

    return (
        await asyncio_detailed(
            client=client,
            body=body,
        )
    ).parsed
