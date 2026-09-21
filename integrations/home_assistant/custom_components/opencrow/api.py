"""HTTP client for the OpenCrow voice API."""

from __future__ import annotations

import asyncio
from dataclasses import dataclass
from typing import Any

from aiohttp import ClientError, ClientSession


class OpenCrowError(Exception):
    """Base OpenCrow API error."""


class OpenCrowConnectionError(OpenCrowError):
    """OpenCrow could not be reached."""


class OpenCrowTimeoutError(OpenCrowConnectionError):
    """OpenCrow did not answer before the client deadline."""


class OpenCrowAuthError(OpenCrowError):
    """OpenCrow rejected the bearer token."""


class OpenCrowResponseError(OpenCrowError):
    """OpenCrow returned an error response."""

    def __init__(self, status: int, code: str, message: str) -> None:
        """Initialize an API response error."""
        super().__init__(message)
        self.status = status
        self.code = code


@dataclass(frozen=True, slots=True)
class OpenCrowTurn:
    """A completed OpenCrow turn."""

    text: str
    delivery: str


class OpenCrowClient:
    """Small async client for an OpenCrow instance."""

    def __init__(self, session: ClientSession, base_url: str, token: str) -> None:
        """Initialize the client."""
        self._session = session
        self._base_url = base_url.rstrip("/")
        self._headers = {"Authorization": f"Bearer {token}"}

    async def async_status(self) -> dict[str, Any]:
        """Validate the endpoint and token."""
        return await self._request("GET", "/v1/status", timeout=10)

    async def async_turn(self, payload: dict[str, Any]) -> OpenCrowTurn:
        """Run one synchronous conversation turn."""
        response = await self._request("POST", "/v1/turn", json=payload, timeout=95)
        return OpenCrowTurn(
            text=str(response.get("text", "")),
            delivery=str(response.get("delivery", "voice")),
        )

    async def _request(
        self,
        method: str,
        path: str,
        *,
        json: dict[str, Any] | None = None,
        timeout: float,
    ) -> dict[str, Any]:
        """Send one authenticated JSON request."""
        try:
            async with asyncio.timeout(timeout):
                async with self._session.request(
                    method,
                    self._base_url + path,
                    headers=self._headers,
                    json=json,
                ) as response:
                    try:
                        body = await response.json(content_type=None)
                    except ValueError as err:
                        raise OpenCrowResponseError(
                            response.status,
                            "invalid_response",
                            "OpenCrow returned invalid JSON.",
                        ) from err
        except TimeoutError as err:
            raise OpenCrowTimeoutError from err
        except ClientError as err:
            raise OpenCrowConnectionError from err

        if response.status == 401:
            raise OpenCrowAuthError

        if response.status >= 400:
            error = body.get("error", {}) if isinstance(body, dict) else {}
            raise OpenCrowResponseError(
                response.status,
                str(error.get("code", "unknown_error")),
                str(error.get("message", "OpenCrow returned an error.")),
            )

        if not isinstance(body, dict):
            raise OpenCrowResponseError(
                response.status, "invalid_response", "OpenCrow returned invalid JSON."
            )

        return body
