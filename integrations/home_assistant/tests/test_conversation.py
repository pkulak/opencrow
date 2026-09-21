"""Tests for the OpenCrow conversation entity."""

from __future__ import annotations

from types import SimpleNamespace
import unittest

from homeassistant.components import conversation
from homeassistant.core import Context

from custom_components.opencrow.api import OpenCrowResponseError, OpenCrowTurn
from custom_components.opencrow.conversation import (
    OpenCrowConversationEntity,
    _speech_for_response_error,
)


class FakeClient:
    """Capture one OpenCrow turn."""

    def __init__(self) -> None:
        self.payload: dict[str, object] | None = None

    async def async_turn(self, payload: dict[str, object]) -> OpenCrowTurn:
        self.payload = payload
        return OpenCrowTurn(text="The lights are on.", delivery="voice")


class FakeChatLog:
    """Capture assistant chat-log content."""

    def __init__(self) -> None:
        self.content: conversation.AssistantContent | None = None

    def async_add_assistant_content_without_tools(
        self, content: conversation.AssistantContent
    ) -> None:
        self.content = content


class OpenCrowConversationTest(unittest.IsolatedAsyncioTestCase):
    """Exercise HA request and response mapping."""

    async def test_turn_mapping(self) -> None:
        """HA metadata reaches OpenCrow and speech returns to HA."""
        client = FakeClient()
        entry = SimpleNamespace(entry_id="entry", runtime_data=client)
        entity = OpenCrowConversationEntity(entry)
        chat_log = FakeChatLog()
        user_input = conversation.ConversationInput(
            text="Turn on the lights",
            context=Context(user_id="phil"),
            conversation_id="conversation",
            device_id=None,
            satellite_id=None,
            language="en",
            agent_id="conversation.opencrow",
        )

        result = await entity._async_handle_message(user_input, chat_log)

        assert client.payload is not None
        self.assertEqual(client.payload["text"], "Turn on the lights")
        request_context = client.payload["context"]
        self.assertEqual(
            request_context,
            {
                "conversation_id": "conversation",
                "language": "en",
                "user_id": "phil",
            },
        )
        self.assertEqual(
            result.response.speech["plain"]["speech"], "The lights are on."
        )
        self.assertEqual(result.conversation_id, "conversation")
        self.assertEqual(chat_log.content.content, "The lights are on.")

    async def test_friendly_errors(self) -> None:
        """Server failures become short spoken messages."""
        self.assertEqual(
            _speech_for_response_error(OpenCrowResponseError(429, "queue_full", "")),
            "I'm already handling another request. Try again in a moment.",
        )
        self.assertEqual(
            _speech_for_response_error(OpenCrowResponseError(504, "timeout", "")),
            "That took too long, so I stopped.",
        )
        self.assertEqual(
            _speech_for_response_error(OpenCrowResponseError(502, "agent_failed", "")),
            "I couldn't complete that request.",
        )


if __name__ == "__main__":
    unittest.main()
