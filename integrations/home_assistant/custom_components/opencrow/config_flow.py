"""Config flow for OpenCrow."""

from __future__ import annotations

import logging
from typing import Any

import voluptuous as vol

from homeassistant import config_entries
from homeassistant.config_entries import ConfigFlowResult
from homeassistant.const import CONF_URL
from homeassistant.helpers.aiohttp_client import async_get_clientsession

from .api import (
    OpenCrowAuthError,
    OpenCrowClient,
    OpenCrowConnectionError,
    OpenCrowResponseError,
)
from .const import CONF_TOKEN, DEFAULT_NAME, DOMAIN

_LOGGER = logging.getLogger(__name__)


class OpenCrowConfigFlow(config_entries.ConfigFlow, domain=DOMAIN):
    """Configure an OpenCrow conversation agent."""

    VERSION = 1

    async def async_step_user(
        self, user_input: dict[str, Any] | None = None
    ) -> ConfigFlowResult:
        """Handle the initial setup step."""
        errors: dict[str, str] = {}

        if user_input is not None:
            url = str(user_input[CONF_URL]).rstrip("/")
            token = str(user_input[CONF_TOKEN])
            client = OpenCrowClient(async_get_clientsession(self.hass), url, token)

            try:
                await client.async_status()
            except OpenCrowAuthError:
                errors["base"] = "invalid_auth"
            except (OpenCrowConnectionError, OpenCrowResponseError):
                errors["base"] = "cannot_connect"
            except Exception:  # noqa: BLE001
                _LOGGER.exception("Unexpected error validating OpenCrow")
                errors["base"] = "unknown"
            else:
                await self.async_set_unique_id(url)
                self._abort_if_unique_id_configured()

                return self.async_create_entry(
                    title=DEFAULT_NAME,
                    data={CONF_URL: url, CONF_TOKEN: token},
                )

        schema = vol.Schema(
            {
                vol.Required(CONF_URL): str,
                vol.Required(CONF_TOKEN): str,
            }
        )

        return self.async_show_form(
            step_id="user",
            data_schema=self.add_suggested_values_to_schema(schema, user_input),
            errors=errors,
        )
