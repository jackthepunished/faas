from http import HTTPStatus
from typing import Any
from urllib.parse import quote

import httpx

from ... import errors
from ...client import AuthenticatedClient, Client
from ...models.app_env_response import AppEnvResponse
from ...models.problem import Problem
from ...models.put_app_env_request import PutAppEnvRequest
from ...types import Response


def _get_kwargs(
    slug: str,
    key: str,
    *,
    body: PutAppEnvRequest,
) -> dict[str, Any]:
    headers: dict[str, Any] = {}

    _kwargs: dict[str, Any] = {
        "method": "put",
        "url": "/v1/apps/{slug}/env/{key}".format(
            slug=quote(str(slug), safe=""),
            key=quote(str(key), safe=""),
        ),
    }

    _kwargs["json"] = body.to_dict()

    headers["Content-Type"] = "application/json"

    _kwargs["headers"] = headers
    return _kwargs


def _parse_response(
    *, client: AuthenticatedClient | Client, response: httpx.Response
) -> AppEnvResponse | Problem | None:
    if response.status_code == 200:
        response_200 = AppEnvResponse.from_dict(response.json())

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

    if response.status_code == 413:
        response_413 = Problem.from_dict(response.json())

        return response_413

    if response.status_code == 429:
        response_429 = Problem.from_dict(response.json())

        return response_429

    if client.raise_on_unexpected_status:
        raise errors.UnexpectedStatus(response.status_code, response.content)
    else:
        return None


def _build_response(
    *, client: AuthenticatedClient | Client, response: httpx.Response
) -> Response[AppEnvResponse | Problem]:
    return Response(
        status_code=HTTPStatus(response.status_code),
        content=response.content,
        headers=response.headers,
        parsed=_parse_response(client=client, response=response),
    )


def sync_detailed(
    slug: str,
    key: str,
    *,
    client: AuthenticatedClient | Client,
    body: PutAppEnvRequest,
) -> Response[AppEnvResponse | Problem]:
    """Set an env var.

     Persists the plaintext value verbatim in the app_envs table (no
    seal step). Env vars are non-sensitive runtime config by contract
    — credentials stay on `/v1/apps/{slug}/secrets/{key}`. Applies on
    next wake (cold-boot OR snapshot-restore); the running instance
    is unaffected.

    Args:
        slug (str):
        key (str):
        body (PutAppEnvRequest): Set an env var: plaintext value (persisted verbatim in app_envs,
            non-sensitive by contract).

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        Response[AppEnvResponse | Problem]
    """

    kwargs = _get_kwargs(
        slug=slug,
        key=key,
        body=body,
    )

    response = client.get_httpx_client().request(
        **kwargs,
    )

    return _build_response(client=client, response=response)


def sync(
    slug: str,
    key: str,
    *,
    client: AuthenticatedClient | Client,
    body: PutAppEnvRequest,
) -> AppEnvResponse | Problem | None:
    """Set an env var.

     Persists the plaintext value verbatim in the app_envs table (no
    seal step). Env vars are non-sensitive runtime config by contract
    — credentials stay on `/v1/apps/{slug}/secrets/{key}`. Applies on
    next wake (cold-boot OR snapshot-restore); the running instance
    is unaffected.

    Args:
        slug (str):
        key (str):
        body (PutAppEnvRequest): Set an env var: plaintext value (persisted verbatim in app_envs,
            non-sensitive by contract).

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        AppEnvResponse | Problem
    """

    return sync_detailed(
        slug=slug,
        key=key,
        client=client,
        body=body,
    ).parsed


async def asyncio_detailed(
    slug: str,
    key: str,
    *,
    client: AuthenticatedClient | Client,
    body: PutAppEnvRequest,
) -> Response[AppEnvResponse | Problem]:
    """Set an env var.

     Persists the plaintext value verbatim in the app_envs table (no
    seal step). Env vars are non-sensitive runtime config by contract
    — credentials stay on `/v1/apps/{slug}/secrets/{key}`. Applies on
    next wake (cold-boot OR snapshot-restore); the running instance
    is unaffected.

    Args:
        slug (str):
        key (str):
        body (PutAppEnvRequest): Set an env var: plaintext value (persisted verbatim in app_envs,
            non-sensitive by contract).

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        Response[AppEnvResponse | Problem]
    """

    kwargs = _get_kwargs(
        slug=slug,
        key=key,
        body=body,
    )

    response = await client.get_async_httpx_client().request(**kwargs)

    return _build_response(client=client, response=response)


async def asyncio(
    slug: str,
    key: str,
    *,
    client: AuthenticatedClient | Client,
    body: PutAppEnvRequest,
) -> AppEnvResponse | Problem | None:
    """Set an env var.

     Persists the plaintext value verbatim in the app_envs table (no
    seal step). Env vars are non-sensitive runtime config by contract
    — credentials stay on `/v1/apps/{slug}/secrets/{key}`. Applies on
    next wake (cold-boot OR snapshot-restore); the running instance
    is unaffected.

    Args:
        slug (str):
        key (str):
        body (PutAppEnvRequest): Set an env var: plaintext value (persisted verbatim in app_envs,
            non-sensitive by contract).

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        AppEnvResponse | Problem
    """

    return (
        await asyncio_detailed(
            slug=slug,
            key=key,
            client=client,
            body=body,
        )
    ).parsed
