"""Tests for the OpenCrow config flow."""

from __future__ import annotations

from types import SimpleNamespace
import unittest
from unittest.mock import AsyncMock, Mock, patch

from homeassistant.const import CONF_URL

from custom_components.opencrow.config_flow import OpenCrowConfigFlow
from custom_components.opencrow.const import CONF_TOKEN


class FakeClient:
    """Successful status client."""

    async def async_status(self) -> dict[str, object]:
        return {"status": "ok", "ready": True}


class OpenCrowConfigFlowTest(unittest.IsolatedAsyncioTestCase):
    """Exercise successful UI configuration."""

    async def test_create_entry(self) -> None:
        """The flow validates and stores a normalized endpoint."""
        flow = OpenCrowConfigFlow()
        flow.hass = SimpleNamespace()
        flow.async_set_unique_id = AsyncMock()
        flow._abort_if_unique_id_configured = Mock()
        flow.async_create_entry = Mock(return_value={"type": "create_entry"})

        with (
            patch(
                "custom_components.opencrow.config_flow.async_get_clientsession",
                return_value=object(),
            ),
            patch(
                "custom_components.opencrow.config_flow.OpenCrowClient",
                return_value=FakeClient(),
            ),
        ):
            result = await flow.async_step_user(
                {CONF_URL: "http://barnaby.home:8787/", CONF_TOKEN: "secret"}
            )

        self.assertEqual(result, {"type": "create_entry"})
        flow.async_set_unique_id.assert_awaited_once_with("http://barnaby.home:8787")
        flow.async_create_entry.assert_called_once_with(
            title="OpenCrow",
            data={
                CONF_URL: "http://barnaby.home:8787",
                CONF_TOKEN: "secret",
            },
        )


if __name__ == "__main__":
    unittest.main()
