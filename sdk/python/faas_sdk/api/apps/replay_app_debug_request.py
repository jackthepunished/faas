from http import HTTPStatus
from typing import Any
from urllib.parse import quote
from uuid import UUID

import httpx

from ... import errors
from ...client import AuthenticatedClient, Client
from ...models.debug_replay_response import DebugReplayResponse
from ...models.problem import Problem
from ...types import Response


def _get_kwargs(
    slug: str,
    req_id: UUID,
) -> dict[str, Any]:

    _kwargs: dict[str, Any] = {
        "method": "post",
        "url": "/v1/apps/{slug}/debug/requests/{req_id}/replay".format(
            slug=quote(str(slug), safe=""),
            req_id=quote(str(req_id), safe=""),
        ),
    }

    return _kwargs


def _parse_response(
    *, client: AuthenticatedClient | Client, response: httpx.Response
) -> DebugReplayResponse | Problem | None:
    if response.status_code == 202:
        response_202 = DebugReplayResponse.from_dict(response.json())

        return response_202

    if response.status_code == 400:
        response_400 = Problem.from_dict(response.json())

        return response_400

    if response.status_code == 401:
        response_401 = Problem.from_dict(response.json())

        return response_401

    if response.status_code == 402:
        response_402 = Problem.from_dict(response.json())

        return response_402

    if response.status_code == 404:
        response_404 = Problem.from_dict(response.json())

        return response_404

    if response.status_code == 429:
        response_429 = Problem.from_dict(response.json())

        return response_429

    if client.raise_on_unexpected_status:
        raise errors.UnexpectedStatus(response.status_code, response.content)
    else:
        return None


def _build_response(
    *, client: AuthenticatedClient | Client, response: httpx.Response
) -> Response[DebugReplayResponse | Problem]:
    return Response(
        status_code=HTTPStatus(response.status_code),
        content=response.content,
        headers=response.headers,
        parsed=_parse_response(client=client, response=response),
    )


def sync_detailed(
    slug: str,
    req_id: UUID,
    *,
    client: AuthenticatedClient | Client,
) -> Response[DebugReplayResponse | Problem]:
    r"""Queue replay of a recorded request (ADR-127 / PR-B stub).

     PR-B returns 202 with `status: \"queued\"`. The mirror
    invocation pipeline lands in issue #72 PR-A2
    (feat-issue-72-traffic-mirror-pr-a2). The response shape
    is stable across PR-B and PR-A2 so customer tooling can
    wire once. Plan-gated by DebugTelemetryEnabled; requires
    ScopesDeployWriteSurface.

    Args:
        slug (str):
        req_id (UUID):

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        Response[DebugReplayResponse | Problem]
    """

    kwargs = _get_kwargs(
        slug=slug,
        req_id=req_id,
    )

    response = client.get_httpx_client().request(
        **kwargs,
    )

    return _build_response(client=client, response=response)


def sync(
    slug: str,
    req_id: UUID,
    *,
    client: AuthenticatedClient | Client,
) -> DebugReplayResponse | Problem | None:
    r"""Queue replay of a recorded request (ADR-127 / PR-B stub).

     PR-B returns 202 with `status: \"queued\"`. The mirror
    invocation pipeline lands in issue #72 PR-A2
    (feat-issue-72-traffic-mirror-pr-a2). The response shape
    is stable across PR-B and PR-A2 so customer tooling can
    wire once. Plan-gated by DebugTelemetryEnabled; requires
    ScopesDeployWriteSurface.

    Args:
        slug (str):
        req_id (UUID):

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        DebugReplayResponse | Problem
    """

    return sync_detailed(
        slug=slug,
        req_id=req_id,
        client=client,
    ).parsed


async def asyncio_detailed(
    slug: str,
    req_id: UUID,
    *,
    client: AuthenticatedClient | Client,
) -> Response[DebugReplayResponse | Problem]:
    r"""Queue replay of a recorded request (ADR-127 / PR-B stub).

     PR-B returns 202 with `status: \"queued\"`. The mirror
    invocation pipeline lands in issue #72 PR-A2
    (feat-issue-72-traffic-mirror-pr-a2). The response shape
    is stable across PR-B and PR-A2 so customer tooling can
    wire once. Plan-gated by DebugTelemetryEnabled; requires
    ScopesDeployWriteSurface.

    Args:
        slug (str):
        req_id (UUID):

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        Response[DebugReplayResponse | Problem]
    """

    kwargs = _get_kwargs(
        slug=slug,
        req_id=req_id,
    )

    response = await client.get_async_httpx_client().request(**kwargs)

    return _build_response(client=client, response=response)


async def asyncio(
    slug: str,
    req_id: UUID,
    *,
    client: AuthenticatedClient | Client,
) -> DebugReplayResponse | Problem | None:
    r"""Queue replay of a recorded request (ADR-127 / PR-B stub).

     PR-B returns 202 with `status: \"queued\"`. The mirror
    invocation pipeline lands in issue #72 PR-A2
    (feat-issue-72-traffic-mirror-pr-a2). The response shape
    is stable across PR-B and PR-A2 so customer tooling can
    wire once. Plan-gated by DebugTelemetryEnabled; requires
    ScopesDeployWriteSurface.

    Args:
        slug (str):
        req_id (UUID):

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        DebugReplayResponse | Problem
    """

    return (
        await asyncio_detailed(
            slug=slug,
            req_id=req_id,
            client=client,
        )
    ).parsed
