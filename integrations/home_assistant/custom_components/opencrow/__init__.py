"""OpenCrow conversation integration."""

from __future__ import annotations

from homeassistant.config_entries import ConfigEntry
from homeassistant.const import CONF_URL, Platform
from homeassistant.core import HomeAssistant
from homeassistant.exceptions import ConfigEntryAuthFailed, ConfigEntryNotReady
from homeassistant.helpers.aiohttp_client import async_get_clientsession

from .api import (
    OpenCrowAuthError,
    OpenCrowClient,
    OpenCrowConnectionError,
    OpenCrowResponseError,
)
from .const import CONF_TOKEN

PLATFORMS = (Platform.CONVERSATION,)

type OpenCrowConfigEntry = ConfigEntry[OpenCrowClient]


async def async_setup_entry(hass: HomeAssistant, entry: OpenCrowConfigEntry) -> bool:
    """Set up OpenCrow from a config entry."""
    client = OpenCrowClient(
        async_get_clientsession(hass),
        entry.data[CONF_URL],
        entry.data[CONF_TOKEN],
    )

    try:
        await client.async_status()
    except OpenCrowAuthError as err:
        raise ConfigEntryAuthFailed from err
    except (OpenCrowConnectionError, OpenCrowResponseError) as err:
        raise ConfigEntryNotReady from err

    entry.runtime_data = client
    await hass.config_entries.async_forward_entry_setups(entry, PLATFORMS)

    return True


async def async_unload_entry(hass: HomeAssistant, entry: OpenCrowConfigEntry) -> bool:
    """Unload OpenCrow."""
    return await hass.config_entries.async_unload_platforms(entry, PLATFORMS)
