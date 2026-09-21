"""Tests for the OpenCrow Home Assistant API client."""

from __future__ import annotations

import unittest

from aiohttp import ClientSession, web

from custom_components.opencrow.api import (
    OpenCrowAuthError,
    OpenCrowClient,
    OpenCrowResponseError,
)


class OpenCrowClientTest(unittest.IsolatedAsyncioTestCase):
    """Exercise the JSON contract without a Home Assistant runtime."""

    async def asyncSetUp(self) -> None:
        """Start a local API stub."""
        self.requests: list[dict[str, object]] = []
        app = web.Application()
        app.router.add_get("/v1/status", self._status)
        app.router.add_post("/v1/turn", self._turn)
        self.runner = web.AppRunner(app)
        await self.runner.setup()
        self.site = web.TCPSite(self.runner, "127.0.0.1", 0)
        await self.site.start()

        sockets = self.site._server.sockets  # noqa: SLF001
        port = sockets[0].getsockname()[1]
        self.session = ClientSession()
        self.client = OpenCrowClient(self.session, f"http://127.0.0.1:{port}", "secret")

    async def asyncTearDown(self) -> None:
        """Stop the API stub."""
        await self.session.close()
        await self.runner.cleanup()

    async def _status(self, request: web.Request) -> web.Response:
        if request.headers.get("Authorization") != "Bearer secret":
            return web.json_response({"error": {"code": "unauthorized"}}, status=401)
        return web.json_response({"status": "ok", "ready": True})

    async def _turn(self, request: web.Request) -> web.Response:
        payload = await request.json()
        self.requests.append(payload)
        if payload["text"] == "busy":
            return web.json_response(
                {"error": {"code": "queue_full", "message": "busy"}}, status=429
            )
        return web.json_response(
            {
                "request_id": payload["request_id"],
                "text": "Done.",
                "delivery": "voice",
            }
        )

    async def test_status_and_turn(self) -> None:
        """The client authenticates and maps a completed turn."""
        status = await self.client.async_status()
        self.assertTrue(status["ready"])

        payload = {
            "request_id": "35cc6ab3-12fe-4bc5-8dda-31f599d5ce78",
            "text": "Turn on the lights",
            "context": {"area_id": "kitchen"},
        }
        turn = await self.client.async_turn(payload)
        self.assertEqual(turn.text, "Done.")
        self.assertEqual(turn.delivery, "voice")
        self.assertEqual(self.requests, [payload])

    async def test_error_mapping(self) -> None:
        """Structured OpenCrow errors keep their status and code."""
        with self.assertRaises(OpenCrowResponseError) as caught:
            await self.client.async_turn(
                {
                    "request_id": "35cc6ab3-12fe-4bc5-8dda-31f599d5ce78",
                    "text": "busy",
                    "context": {},
                }
            )
        self.assertEqual(caught.exception.status, 429)
        self.assertEqual(caught.exception.code, "queue_full")

    async def test_auth_error(self) -> None:
        """A rejected token has a distinct exception."""
        bad_client = OpenCrowClient(
            self.session,
            self.client._base_url,
            "wrong",  # noqa: SLF001
        )
        with self.assertRaises(OpenCrowAuthError):
            await bad_client.async_status()


if __name__ == "__main__":
    unittest.main()
