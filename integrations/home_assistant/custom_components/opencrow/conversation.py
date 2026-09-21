"""Conversation entity for OpenCrow."""

from __future__ import annotations

import logging
from typing import Literal, override
from uuid import uuid4

from homeassistant.components import conversation
from homeassistant.const import MATCH_ALL
from homeassistant.core import HomeAssistant
from homeassistant.helpers import device_registry as dr, intent
from homeassistant.helpers.entity_platform import AddConfigEntryEntitiesCallback

from . import OpenCrowConfigEntry
from .api import (
    OpenCrowAuthError,
    OpenCrowConnectionError,
    OpenCrowResponseError,
    OpenCrowTimeoutError,
)
from .const import DEFAULT_NAME

_LOGGER = logging.getLogger(__name__)


async def async_setup_entry(
    hass: HomeAssistant,
    entry: OpenCrowConfigEntry,
    async_add_entities: AddConfigEntryEntitiesCallback,
) -> None:
    """Set up the OpenCrow conversation entity."""
    async_add_entities([OpenCrowConversationEntity(entry)])


class OpenCrowConversationEntity(
    conversation.ConversationEntity,
    conversation.AbstractConversationAgent,
):
    """OpenCrow conversation agent."""

    _attr_has_entity_name = True
    _attr_name = DEFAULT_NAME
    _attr_supported_features = conversation.ConversationEntityFeature.CONTROL

    def __init__(self, entry: OpenCrowConfigEntry) -> None:
        """Initialize the conversation entity."""
        self._entry = entry
        self._attr_unique_id = entry.entry_id

    @property
    @override
    def supported_languages(self) -> list[str] | Literal["*"]:
        """Return the languages supported by OpenCrow."""
        return MATCH_ALL

    @override
    async def async_added_to_hass(self) -> None:
        """Register the agent when the entity is added."""
        await super().async_added_to_hass()
        conversation.async_set_agent(self.hass, self._entry, self)

    @override
    async def async_will_remove_from_hass(self) -> None:
        """Unregister the agent when the entity is removed."""
        conversation.async_unset_agent(self.hass, self._entry)
        await super().async_will_remove_from_hass()

    @override
    async def _async_handle_message(
        self,
        user_input: conversation.ConversationInput,
        chat_log: conversation.ChatLog,
    ) -> conversation.ConversationResult:
        """Forward one text turn to OpenCrow."""
        payload = {
            "request_id": str(uuid4()),
            "text": user_input.text,
            "context": self._request_context(user_input),
        }

        try:
            turn = await self._entry.runtime_data.async_turn(payload)
            speech = turn.text
        except OpenCrowResponseError as err:
            _LOGGER.warning(
                "OpenCrow rejected a voice turn: %s (%s)", err.code, err.status
            )
            speech = _speech_for_response_error(err)
        except OpenCrowAuthError:
            _LOGGER.error("OpenCrow rejected the configured bearer token")
            speech = "OpenCrow's authentication needs to be fixed."
        except OpenCrowTimeoutError:
            _LOGGER.warning("OpenCrow did not answer before the client deadline")
            speech = "That took too long, so I stopped."
        except OpenCrowConnectionError:
            _LOGGER.warning("OpenCrow could not be reached")
            speech = "I can't reach OpenCrow right now."

        chat_log.async_add_assistant_content_without_tools(
            conversation.AssistantContent(
                agent_id=user_input.agent_id,
                content=speech,
            )
        )

        response = intent.IntentResponse(language=user_input.language)
        response.async_set_speech(speech)

        return conversation.ConversationResult(
            conversation_id=user_input.conversation_id,
            response=response,
            continue_conversation=False,
        )

    def _request_context(
        self, user_input: conversation.ConversationInput
    ) -> dict[str, str]:
        """Build the trusted HA metadata sent with a turn."""
        values = {
            "conversation_id": user_input.conversation_id,
            "device_id": user_input.device_id or user_input.satellite_id,
            "language": user_input.language,
            "user_id": user_input.context.user_id,
        }

        device_id = user_input.device_id or user_input.satellite_id
        if device_id:
            device = dr.async_get(self.hass).async_get(device_id)
            if device is not None:
                values["area_id"] = device.area_id

        return {key: value for key, value in values.items() if value is not None}


def _speech_for_response_error(err: OpenCrowResponseError) -> str:
    """Turn API failures into short spoken responses."""
    if err.status == 429:
        return "I'm already handling another request. Try again in a moment."
    if err.status == 504 or err.code == "timeout":
        return "That took too long, so I stopped."

    return "I couldn't complete that request."
