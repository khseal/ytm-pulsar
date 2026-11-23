"""Voice handlers. The phrases avoid the words the speaker handles itself:
дальше, следующий, назад, стоп, пауза, продолжи, играй, громче, тише."""

from __future__ import annotations

from homeassistant.core import HomeAssistant
from homeassistant.helpers.intent import (
    Intent,
    IntentHandler,
    IntentResponse,
    async_register,
    async_remove,
)

from .const import (
    INTENT_CLEAR,
    INTENT_NEXT,
    INTENT_PLAY,
    INTENT_PREVIOUS,
    INTENT_RANDOM,
    INTENT_SEARCH,
    INTENTS,
)


def register(hass: HomeAssistant) -> None:
    """Register the YouTube phrases. Repeating this replaces the same intents."""
    for handler in HANDLERS:
        async_register(hass, handler())


def unregister(hass: HomeAssistant) -> None:
    """Drop the YouTube phrases."""
    for name in INTENTS:
        async_remove(hass, name)


def _speech(intent_obj: Intent, text: str) -> IntentResponse:
    response = intent_obj.create_response()
    response.async_set_speech(text)
    return response


def _query(intent_obj: Intent) -> str:
    slot = intent_obj.slots.get("ytm_query")
    if slot is None:
        return ""
    value = slot.get("value") if isinstance(slot, dict) else getattr(slot, "value", "")
    return str(value or "").strip()


class _PlayerIntent(IntentHandler):
    async def _player(self, intent_obj: Intent):
        from . import player

        entity = player(intent_obj.hass)
        if entity is None:
            return None, _speech(intent_obj, "Ютуб не настроен")
        return entity, None


class PlayIntent(_PlayerIntent):
    intent_type = INTENT_PLAY
    description = "Включить музыку с YouTube"

    async def async_handle(self, intent_obj: Intent) -> IntentResponse:
        entity, missing = await self._player(intent_obj)
        if missing:
            return missing
        return _speech(intent_obj, await entity.async_play_youtube())


class NextIntent(_PlayerIntent):
    intent_type = INTENT_NEXT
    description = "Следующий трек YouTube"

    async def async_handle(self, intent_obj: Intent) -> IntentResponse:
        entity, missing = await self._player(intent_obj)
        if missing:
            return missing
        return _speech(intent_obj, await entity.async_voice_next())


class PreviousIntent(_PlayerIntent):
    intent_type = INTENT_PREVIOUS
    description = "Предыдущий трек YouTube"

    async def async_handle(self, intent_obj: Intent) -> IntentResponse:
        entity, missing = await self._player(intent_obj)
        if missing:
            return missing
        return _speech(intent_obj, await entity.async_voice_previous())


class SearchIntent(_PlayerIntent):
    intent_type = INTENT_SEARCH
    description = "Найти на YouTube и включить"

    async def async_handle(self, intent_obj: Intent) -> IntentResponse:
        entity, missing = await self._player(intent_obj)
        if missing:
            return missing
        return _speech(intent_obj, await entity.async_voice_search(_query(intent_obj)))


class ClearIntent(_PlayerIntent):
    intent_type = INTENT_CLEAR
    description = "Очистить очередь YouTube"

    async def async_handle(self, intent_obj: Intent) -> IntentResponse:
        entity, missing = await self._player(intent_obj)
        if missing:
            return missing
        return _speech(intent_obj, await entity.async_voice_clear())


class RandomIntent(_PlayerIntent):
    intent_type = INTENT_RANDOM
    description = "Очистить очередь и включить случайные треки YouTube"

    async def async_handle(self, intent_obj: Intent) -> IntentResponse:
        entity, missing = await self._player(intent_obj)
        if missing:
            return missing
        return _speech(intent_obj, await entity.async_voice_random())


HANDLERS = (PlayIntent, NextIntent, PreviousIntent, SearchIntent, ClearIntent, RandomIntent)
