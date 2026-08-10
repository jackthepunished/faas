from http import HTTPStatus
from typing import Any
from urllib.parse import quote

import httpx

from ... import errors
from ...client import AuthenticatedClient, Client
from ...models.async_invoke_response import AsyncInvokeResponse
from ...models.problem import Problem
from ...types import Response


def _get_kwargs(
    id: str,
) -> dict[str, Any]:

    _kwargs: dict[str, Any] = {
        "method": "post",
        "url": "/v1/invocations/{id}/replay".format(
            id=quote(str(id), safe=""),
        ),
    }

    return _kwargs


def _parse_response(
    *, client: AuthenticatedClient | Client, response: httpx.Response
) -> AsyncInvokeResponse | Problem | None:
    if response.status_code == 202:
        response_202 = AsyncInvokeResponse.from_dict(response.json())

        return response_202

    if response.status_code == 401:
        response_401 = Problem.from_dict(response.json())

        return response_401

    if response.status_code == 404:
        response_404 = Problem.from_dict(response.json())

        return response_404

    if response.status_code == 409:
        response_409 = Problem.from_dict(response.json())

        return response_409

    if response.status_code == 429:
        response_429 = Problem.from_dict(response.json())

        return response_429

    if client.raise_on_unexpected_status:
        raise errors.UnexpectedStatus(response.status_code, response.content)
    else:
        return None


def _build_response(
    *, client: AuthenticatedClient | Client, response: httpx.Response
) -> Response[AsyncInvokeResponse | Problem]:
    return Response(
        status_code=HTTPStatus(response.status_code),
        content=response.content,
        headers=response.headers,
        parsed=_parse_response(client=client, response=response),
    )


def sync_detailed(
    id: str,
    *,
    client: AuthenticatedClient | Client,
) -> Response[AsyncInvokeResponse | Problem]:
    """Re-issue a failed or dead_letter invocation.

     Accepts no request body. The replayed row carries the original's
    payload + headers + method + path verbatim and is enqueued against
    the same app. The new row's Source is `replay` (issue #315).

    Only invocations whose current state is `failed` or `dead_letter`
    can be replayed — re-running a successful or in-flight invocation
    would be a customer bug, not a flow we want to enable by accident.
    A replay attempt on any other state returns 409
    `invocation_not_replayable`.

    Args:
        id (str):

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        Response[AsyncInvokeResponse | Problem]
    """

    kwargs = _get_kwargs(
        id=id,
    )

    response = client.get_httpx_client().request(
        **kwargs,
    )

    return _build_response(client=client, response=response)


def sync(
    id: str,
    *,
    client: AuthenticatedClient | Client,
) -> AsyncInvokeResponse | Problem | None:
    """Re-issue a failed or dead_letter invocation.

     Accepts no request body. The replayed row carries the original's
    payload + headers + method + path verbatim and is enqueued against
    the same app. The new row's Source is `replay` (issue #315).

    Only invocations whose current state is `failed` or `dead_letter`
    can be replayed — re-running a successful or in-flight invocation
    would be a customer bug, not a flow we want to enable by accident.
    A replay attempt on any other state returns 409
    `invocation_not_replayable`.

    Args:
        id (str):

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        AsyncInvokeResponse | Problem
    """

    return sync_detailed(
        id=id,
        client=client,
    ).parsed


async def asyncio_detailed(
    id: str,
    *,
    client: AuthenticatedClient | Client,
) -> Response[AsyncInvokeResponse | Problem]:
    """Re-issue a failed or dead_letter invocation.

     Accepts no request body. The replayed row carries the original's
    payload + headers + method + path verbatim and is enqueued against
    the same app. The new row's Source is `replay` (issue #315).

    Only invocations whose current state is `failed` or `dead_letter`
    can be replayed — re-running a successful or in-flight invocation
    would be a customer bug, not a flow we want to enable by accident.
    A replay attempt on any other state returns 409
    `invocation_not_replayable`.

    Args:
        id (str):

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        Response[AsyncInvokeResponse | Problem]
    """

    kwargs = _get_kwargs(
        id=id,
    )

    response = await client.get_async_httpx_client().request(**kwargs)

    return _build_response(client=client, response=response)


async def asyncio(
    id: str,
    *,
    client: AuthenticatedClient | Client,
) -> AsyncInvokeResponse | Problem | None:
    """Re-issue a failed or dead_letter invocation.

     Accepts no request body. The replayed row carries the original's
    payload + headers + method + path verbatim and is enqueued against
    the same app. The new row's Source is `replay` (issue #315).

    Only invocations whose current state is `failed` or `dead_letter`
    can be replayed — re-running a successful or in-flight invocation
    would be a customer bug, not a flow we want to enable by accident.
    A replay attempt on any other state returns 409
    `invocation_not_replayable`.

    Args:
        id (str):

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        AsyncInvokeResponse | Problem
    """

    return (
        await asyncio_detailed(
            id=id,
            client=client,
        )
    ).parsed
