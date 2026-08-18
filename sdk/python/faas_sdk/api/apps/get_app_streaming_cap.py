from http import HTTPStatus
from typing import Any
from urllib.parse import quote

import httpx

from ... import errors
from ...client import AuthenticatedClient, Client
from ...models.app_streaming_status import AppStreamingStatus
from ...models.problem import Problem
from ...types import Response


def _get_kwargs(
    slug: str,
) -> dict[str, Any]:

    _kwargs: dict[str, Any] = {
        "method": "get",
        "url": "/v1/apps/{slug}/streaming-cap".format(
            slug=quote(str(slug), safe=""),
        ),
    }

    return _kwargs


def _parse_response(
    *, client: AuthenticatedClient | Client, response: httpx.Response
) -> AppStreamingStatus | Problem | None:
    if response.status_code == 200:
        response_200 = AppStreamingStatus.from_dict(response.json())

        return response_200

    if response.status_code == 400:
        response_400 = Problem.from_dict(response.json())

        return response_400

    if response.status_code == 401:
        response_401 = Problem.from_dict(response.json())

        return response_401

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
) -> Response[AppStreamingStatus | Problem]:
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
) -> Response[AppStreamingStatus | Problem]:
    r"""Per-app streaming classification probe (ADR-102 D6).

     Returns the streaming-status enum (one of `streaming`,
    `accept-json-downgrade`, `flag-disabled`, `plan-disallows`,
    `operator-disabled`, `upgrade-bypass`) the gatewayd handler
    would stamp on the `Streaming-Status` response header for a
    representative request to this app, plus the effective
    response-body cap (in bytes) and the per-gate flags.

    The probe is a pure read against the apid cache (the
    per-account `Plan` and the per-app `streaming_enabled`
    flag). It does NOT dial gatewayd-internal — the operator
    opt-in (`FAAS_GATEWAY_STREAMING` env) and per-edge-rule
    cap override are gatewayd-side state, so `effective_cap_bytes`
    reflects the plan cap (`cap_kind=\"plan\"`) on every probe.
    A customer evaluating \"will my next request stream?\" must
    consider the operator-side flag separately; the canonical
    signal is the `Streaming-Status` response header on a real
    request, not this probe.

    `status=plan-disallows` means the customer's plan tier
    forbids `streaming_enabled=true`; the CreateApp gate (D5)
    already returns 403 `CodePlanStreamingNotAllowed` so this
    row should be unreachable from a properly-validated app,
    but the probe still reflects the persisted state for
    audits and pinned-SDK migrations.

    Args:
        slug (str):

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        Response[AppStreamingStatus | Problem]
    """

    kwargs = _get_kwargs(
        slug=slug,
    )

    response = client.get_httpx_client().request(
        **kwargs,
    )

    return _build_response(client=client, response=response)


def sync(
    slug: str,
    *,
    client: AuthenticatedClient | Client,
) -> AppStreamingStatus | Problem | None:
    r"""Per-app streaming classification probe (ADR-102 D6).

     Returns the streaming-status enum (one of `streaming`,
    `accept-json-downgrade`, `flag-disabled`, `plan-disallows`,
    `operator-disabled`, `upgrade-bypass`) the gatewayd handler
    would stamp on the `Streaming-Status` response header for a
    representative request to this app, plus the effective
    response-body cap (in bytes) and the per-gate flags.

    The probe is a pure read against the apid cache (the
    per-account `Plan` and the per-app `streaming_enabled`
    flag). It does NOT dial gatewayd-internal — the operator
    opt-in (`FAAS_GATEWAY_STREAMING` env) and per-edge-rule
    cap override are gatewayd-side state, so `effective_cap_bytes`
    reflects the plan cap (`cap_kind=\"plan\"`) on every probe.
    A customer evaluating \"will my next request stream?\" must
    consider the operator-side flag separately; the canonical
    signal is the `Streaming-Status` response header on a real
    request, not this probe.

    `status=plan-disallows` means the customer's plan tier
    forbids `streaming_enabled=true`; the CreateApp gate (D5)
    already returns 403 `CodePlanStreamingNotAllowed` so this
    row should be unreachable from a properly-validated app,
    but the probe still reflects the persisted state for
    audits and pinned-SDK migrations.

    Args:
        slug (str):

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        AppStreamingStatus | Problem
    """

    return sync_detailed(
        slug=slug,
        client=client,
    ).parsed


async def asyncio_detailed(
    slug: str,
    *,
    client: AuthenticatedClient | Client,
) -> Response[AppStreamingStatus | Problem]:
    r"""Per-app streaming classification probe (ADR-102 D6).

     Returns the streaming-status enum (one of `streaming`,
    `accept-json-downgrade`, `flag-disabled`, `plan-disallows`,
    `operator-disabled`, `upgrade-bypass`) the gatewayd handler
    would stamp on the `Streaming-Status` response header for a
    representative request to this app, plus the effective
    response-body cap (in bytes) and the per-gate flags.

    The probe is a pure read against the apid cache (the
    per-account `Plan` and the per-app `streaming_enabled`
    flag). It does NOT dial gatewayd-internal — the operator
    opt-in (`FAAS_GATEWAY_STREAMING` env) and per-edge-rule
    cap override are gatewayd-side state, so `effective_cap_bytes`
    reflects the plan cap (`cap_kind=\"plan\"`) on every probe.
    A customer evaluating \"will my next request stream?\" must
    consider the operator-side flag separately; the canonical
    signal is the `Streaming-Status` response header on a real
    request, not this probe.

    `status=plan-disallows` means the customer's plan tier
    forbids `streaming_enabled=true`; the CreateApp gate (D5)
    already returns 403 `CodePlanStreamingNotAllowed` so this
    row should be unreachable from a properly-validated app,
    but the probe still reflects the persisted state for
    audits and pinned-SDK migrations.

    Args:
        slug (str):

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        Response[AppStreamingStatus | Problem]
    """

    kwargs = _get_kwargs(
        slug=slug,
    )

    response = await client.get_async_httpx_client().request(**kwargs)

    return _build_response(client=client, response=response)


async def asyncio(
    slug: str,
    *,
    client: AuthenticatedClient | Client,
) -> AppStreamingStatus | Problem | None:
    r"""Per-app streaming classification probe (ADR-102 D6).

     Returns the streaming-status enum (one of `streaming`,
    `accept-json-downgrade`, `flag-disabled`, `plan-disallows`,
    `operator-disabled`, `upgrade-bypass`) the gatewayd handler
    would stamp on the `Streaming-Status` response header for a
    representative request to this app, plus the effective
    response-body cap (in bytes) and the per-gate flags.

    The probe is a pure read against the apid cache (the
    per-account `Plan` and the per-app `streaming_enabled`
    flag). It does NOT dial gatewayd-internal — the operator
    opt-in (`FAAS_GATEWAY_STREAMING` env) and per-edge-rule
    cap override are gatewayd-side state, so `effective_cap_bytes`
    reflects the plan cap (`cap_kind=\"plan\"`) on every probe.
    A customer evaluating \"will my next request stream?\" must
    consider the operator-side flag separately; the canonical
    signal is the `Streaming-Status` response header on a real
    request, not this probe.

    `status=plan-disallows` means the customer's plan tier
    forbids `streaming_enabled=true`; the CreateApp gate (D5)
    already returns 403 `CodePlanStreamingNotAllowed` so this
    row should be unreachable from a properly-validated app,
    but the probe still reflects the persisted state for
    audits and pinned-SDK migrations.

    Args:
        slug (str):

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        AppStreamingStatus | Problem
    """

    return (
        await asyncio_detailed(
            slug=slug,
            client=client,
        )
    ).parsed
