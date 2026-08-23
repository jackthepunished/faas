from http import HTTPStatus
from typing import Any
from urllib.parse import quote

import httpx

from ... import errors
from ...client import AuthenticatedClient, Client
from ...models.deployment_response import DeploymentResponse
from ...models.problem import Problem
from ...models.retry_deployment_request import RetryDeploymentRequest
from ...types import Response


def _get_kwargs(
    id: str,
    *,
    body: RetryDeploymentRequest,
) -> dict[str, Any]:
    headers: dict[str, Any] = {}

    _kwargs: dict[str, Any] = {
        "method": "post",
        "url": "/v1/deployments/{id}/retry".format(
            id=quote(str(id), safe=""),
        ),
    }

    _kwargs["json"] = body.to_dict()

    headers["Content-Type"] = "application/json"

    _kwargs["headers"] = headers
    return _kwargs


def _parse_response(
    *, client: AuthenticatedClient | Client, response: httpx.Response
) -> DeploymentResponse | Problem | None:
    if response.status_code == 202:
        response_202 = DeploymentResponse.from_dict(response.json())

        return response_202

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
) -> Response[DeploymentResponse | Problem]:
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
    body: RetryDeploymentRequest,
) -> Response[DeploymentResponse | Problem]:
    r"""Per-stage retry (ADR-117 §Production-ready follow-on, C2).

     Inserts a fresh `deployments` row from a failed (or stale)
    row, restarting the imaged chokepoint at `from_stage`. The
    fresh row copies every input primitive from the source
    (`image`, `source_url`, `commit_sha`, `overrides`,
    `sidecars`, `scope`, `traffic_percent`) and seeds a fresh
    `stage_state` (`current = from_stage`, empty history). The
    source row is left untouched — the new row's id is
    returned in the body so the customer can wire it to
    `GET /v1/deployments/{new-id}/logs` for live progress
    (mirrors `gregale deploys retry` UX).

    `from_stage` must be one of the closed 6-stage vocabulary
    (`source_download` / `dependency_restore` / `image_build` /
    `security_scan` / `snapshot_prepare` / `readiness`).
    `from_stage = source_download` is intentional — it's the
    \"retry-from-top\" case and re-runs the whole pipeline.

    Status codes:

      - 202 Accepted with the new row (the row is written;
        imaged picks it up; the SSE stream on the new id is
        the customer's progress surface).
      - 400 if `from_stage` is empty or not in the closed-6
        vocabulary.
      - 401 if not authenticated.
      - 404 if the source deployment does not exist OR is in
        another account (IDOR posture; never 403, never
        reveal cross-account existence).
      - 500 for storage-layer failures.

    Auth chain: authLimited → requireMFA → requireScope
    (deploy-write). Non-idempotent (every call creates a fresh
    row); the operator must surface this in the dashboard.

    Args:
        id (str):
        body (RetryDeploymentRequest): Body for POST /v1/deployments/{id}/retry. Identifies the
            stage the retry should resume from. The closed-6 vocabulary mirrors `state.AllStageNames`
            (ADR-117); the API rejects unknown values with 400. Empty strings are rejected for the
            same reason.

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        Response[DeploymentResponse | Problem]
    """

    kwargs = _get_kwargs(
        id=id,
        body=body,
    )

    response = client.get_httpx_client().request(
        **kwargs,
    )

    return _build_response(client=client, response=response)


def sync(
    id: str,
    *,
    client: AuthenticatedClient | Client,
    body: RetryDeploymentRequest,
) -> DeploymentResponse | Problem | None:
    r"""Per-stage retry (ADR-117 §Production-ready follow-on, C2).

     Inserts a fresh `deployments` row from a failed (or stale)
    row, restarting the imaged chokepoint at `from_stage`. The
    fresh row copies every input primitive from the source
    (`image`, `source_url`, `commit_sha`, `overrides`,
    `sidecars`, `scope`, `traffic_percent`) and seeds a fresh
    `stage_state` (`current = from_stage`, empty history). The
    source row is left untouched — the new row's id is
    returned in the body so the customer can wire it to
    `GET /v1/deployments/{new-id}/logs` for live progress
    (mirrors `gregale deploys retry` UX).

    `from_stage` must be one of the closed 6-stage vocabulary
    (`source_download` / `dependency_restore` / `image_build` /
    `security_scan` / `snapshot_prepare` / `readiness`).
    `from_stage = source_download` is intentional — it's the
    \"retry-from-top\" case and re-runs the whole pipeline.

    Status codes:

      - 202 Accepted with the new row (the row is written;
        imaged picks it up; the SSE stream on the new id is
        the customer's progress surface).
      - 400 if `from_stage` is empty or not in the closed-6
        vocabulary.
      - 401 if not authenticated.
      - 404 if the source deployment does not exist OR is in
        another account (IDOR posture; never 403, never
        reveal cross-account existence).
      - 500 for storage-layer failures.

    Auth chain: authLimited → requireMFA → requireScope
    (deploy-write). Non-idempotent (every call creates a fresh
    row); the operator must surface this in the dashboard.

    Args:
        id (str):
        body (RetryDeploymentRequest): Body for POST /v1/deployments/{id}/retry. Identifies the
            stage the retry should resume from. The closed-6 vocabulary mirrors `state.AllStageNames`
            (ADR-117); the API rejects unknown values with 400. Empty strings are rejected for the
            same reason.

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        DeploymentResponse | Problem
    """

    return sync_detailed(
        id=id,
        client=client,
        body=body,
    ).parsed


async def asyncio_detailed(
    id: str,
    *,
    client: AuthenticatedClient | Client,
    body: RetryDeploymentRequest,
) -> Response[DeploymentResponse | Problem]:
    r"""Per-stage retry (ADR-117 §Production-ready follow-on, C2).

     Inserts a fresh `deployments` row from a failed (or stale)
    row, restarting the imaged chokepoint at `from_stage`. The
    fresh row copies every input primitive from the source
    (`image`, `source_url`, `commit_sha`, `overrides`,
    `sidecars`, `scope`, `traffic_percent`) and seeds a fresh
    `stage_state` (`current = from_stage`, empty history). The
    source row is left untouched — the new row's id is
    returned in the body so the customer can wire it to
    `GET /v1/deployments/{new-id}/logs` for live progress
    (mirrors `gregale deploys retry` UX).

    `from_stage` must be one of the closed 6-stage vocabulary
    (`source_download` / `dependency_restore` / `image_build` /
    `security_scan` / `snapshot_prepare` / `readiness`).
    `from_stage = source_download` is intentional — it's the
    \"retry-from-top\" case and re-runs the whole pipeline.

    Status codes:

      - 202 Accepted with the new row (the row is written;
        imaged picks it up; the SSE stream on the new id is
        the customer's progress surface).
      - 400 if `from_stage` is empty or not in the closed-6
        vocabulary.
      - 401 if not authenticated.
      - 404 if the source deployment does not exist OR is in
        another account (IDOR posture; never 403, never
        reveal cross-account existence).
      - 500 for storage-layer failures.

    Auth chain: authLimited → requireMFA → requireScope
    (deploy-write). Non-idempotent (every call creates a fresh
    row); the operator must surface this in the dashboard.

    Args:
        id (str):
        body (RetryDeploymentRequest): Body for POST /v1/deployments/{id}/retry. Identifies the
            stage the retry should resume from. The closed-6 vocabulary mirrors `state.AllStageNames`
            (ADR-117); the API rejects unknown values with 400. Empty strings are rejected for the
            same reason.

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        Response[DeploymentResponse | Problem]
    """

    kwargs = _get_kwargs(
        id=id,
        body=body,
    )

    response = await client.get_async_httpx_client().request(**kwargs)

    return _build_response(client=client, response=response)


async def asyncio(
    id: str,
    *,
    client: AuthenticatedClient | Client,
    body: RetryDeploymentRequest,
) -> DeploymentResponse | Problem | None:
    r"""Per-stage retry (ADR-117 §Production-ready follow-on, C2).

     Inserts a fresh `deployments` row from a failed (or stale)
    row, restarting the imaged chokepoint at `from_stage`. The
    fresh row copies every input primitive from the source
    (`image`, `source_url`, `commit_sha`, `overrides`,
    `sidecars`, `scope`, `traffic_percent`) and seeds a fresh
    `stage_state` (`current = from_stage`, empty history). The
    source row is left untouched — the new row's id is
    returned in the body so the customer can wire it to
    `GET /v1/deployments/{new-id}/logs` for live progress
    (mirrors `gregale deploys retry` UX).

    `from_stage` must be one of the closed 6-stage vocabulary
    (`source_download` / `dependency_restore` / `image_build` /
    `security_scan` / `snapshot_prepare` / `readiness`).
    `from_stage = source_download` is intentional — it's the
    \"retry-from-top\" case and re-runs the whole pipeline.

    Status codes:

      - 202 Accepted with the new row (the row is written;
        imaged picks it up; the SSE stream on the new id is
        the customer's progress surface).
      - 400 if `from_stage` is empty or not in the closed-6
        vocabulary.
      - 401 if not authenticated.
      - 404 if the source deployment does not exist OR is in
        another account (IDOR posture; never 403, never
        reveal cross-account existence).
      - 500 for storage-layer failures.

    Auth chain: authLimited → requireMFA → requireScope
    (deploy-write). Non-idempotent (every call creates a fresh
    row); the operator must surface this in the dashboard.

    Args:
        id (str):
        body (RetryDeploymentRequest): Body for POST /v1/deployments/{id}/retry. Identifies the
            stage the retry should resume from. The closed-6 vocabulary mirrors `state.AllStageNames`
            (ADR-117); the API rejects unknown values with 400. Empty strings are rejected for the
            same reason.

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        DeploymentResponse | Problem
    """

    return (
        await asyncio_detailed(
            id=id,
            client=client,
            body=body,
        )
    ).parsed
