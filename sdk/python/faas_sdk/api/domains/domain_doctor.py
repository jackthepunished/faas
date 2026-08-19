from http import HTTPStatus
from typing import Any, cast
from urllib.parse import quote

import httpx

from ... import errors
from ...client import AuthenticatedClient, Client
from ...models.domain_doctor_report import DomainDoctorReport
from ...models.problem import Problem
from ...types import Response


def _get_kwargs(
    domain: str,
) -> dict[str, Any]:

    _kwargs: dict[str, Any] = {
        "method": "get",
        "url": "/v1/domains/{domain}/doctor".format(
            domain=quote(str(domain), safe=""),
        ),
    }

    return _kwargs


def _parse_response(
    *, client: AuthenticatedClient | Client, response: httpx.Response
) -> DomainDoctorReport | Problem | None:
    if response.status_code == 200:
        response_200 = DomainDoctorReport.from_dict(response.json())

        return response_200

    if response.status_code == 401:
        response_401 = Problem.from_dict(response.json())

        return response_401

    if response.status_code == 404:
        response_404 = Problem.from_dict(response.json())

        return response_404

    if response.status_code == 429:
        response_429 = Problem.from_dict(response.json())

        return response_429

    if response.status_code == 503:
        response_503 = Problem.from_dict(response.json())

        return response_503

    if client.raise_on_unexpected_status:
        raise errors.UnexpectedStatus(response.status_code, response.content)
    else:
        return None


def _build_response(
    *, client: AuthenticatedClient | Client, response: httpx.Response
) -> Response[DomainDoctorReport | Problem]:
    return Response(
        status_code=HTTPStatus(response.status_code),
        content=response.content,
        headers=response.headers,
        parsed=_parse_response(client=client, response=response),
    )


def sync_detailed(
    domain: str,
    *,
    client: AuthenticatedClient | Client,
) -> Response[DomainDoctorReport | Problem]:
    """Doctor a domain (ADR-120).

    Returns the 5-check doctor report (DNS record found / points to Gregale / TLS certificate /
    CAA permits / IPv6 conflict) with a human-readable remediation line per failing check. Backed
    by GET /v1/domains/{domain}/doctor. The handler reads the latest observation row from
    domain_doctor_observations; on a stale or missing row it triggers a synchronous re-probe
    with a 5s budget.

    Args:
        domain (str):

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        Response[DomainDoctorReport | Problem]
    """

    kwargs = _get_kwargs(
        domain=domain,
    )

    response = client.get_httpx_client().request(
        **kwargs,
    )

    return _build_response(client=client, response=response)


def sync(
    domain: str,
    *,
    client: AuthenticatedClient | Client,
) -> DomainDoctorReport | Problem | None:
    """Doctor a domain (ADR-120).

    Returns the 5-check doctor report (DNS record found / points to Gregale / TLS certificate /
    CAA permits / IPv6 conflict) with a human-readable remediation line per failing check. Backed
    by GET /v1/domains/{domain}/doctor.

    Args:
        domain (str):

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        DomainDoctorReport | Problem
    """

    return sync_detailed(
        domain=domain,
        client=client,
    ).parsed


async def asyncio_detailed(
    domain: str,
    *,
    client: AuthenticatedClient | Client,
) -> Response[DomainDoctorReport | Problem]:
    """Doctor a domain (ADR-120).

    Returns the 5-check doctor report (DNS record found / points to Gregale / TLS certificate /
    CAA permits / IPv6 conflict) with a human-readable remediation line per failing check. Backed
    by GET /v1/domains/{domain}/doctor.

    Args:
        domain (str):

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        Response[DomainDoctorReport | Problem]
    """

    kwargs = _get_kwargs(
        domain=domain,
    )

    response = await client.get_async_httpx_client().request(**kwargs)

    return _build_response(client=client, response=response)


async def asyncio(
    domain: str,
    *,
    client: AuthenticatedClient | Client,
) -> DomainDoctorReport | Problem | None:
    """Doctor a domain (ADR-120).

    Returns the 5-check doctor report (DNS record found / points to Gregale / TLS certificate /
    CAA permits / IPv6 conflict) with a human-readable remediation line per failing check. Backed
    by GET /v1/domains/{domain}/doctor.

    Args:
        domain (str):

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        DomainDoctorReport | Problem
    """

    return (
        await asyncio_detailed(
            domain=domain,
            client=client,
        )
    ).parsed