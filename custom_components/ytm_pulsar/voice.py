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
    INTENT_FORWARD,
    INTENT_NEXT,
    INTENT_PLAY,
    INTENT_PREVIOUS,
    INTENT_RANDOM,
    INTENT_REWIND,
    INTENT_SEARCH,
    INTENT_SPEAKER_OFF,
    INTENT_SPEAKER_ON,
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


def _slot(intent_obj: Intent, name: str) -> str:
    slot = intent_obj.slots.get(name)
    if slot is None:
        return ""
    value = slot.get("value") if isinstance(slot, dict) else getattr(slot, "value", "")
    return str(value or "").strip()


def _query(intent_obj: Intent) -> str:
    return _slot(intent_obj, "query") or _slot(intent_obj, "ytm_query")


def _search_schema():
    """Parameter for the language model. Exact phrases still use ytm_query."""
    try:
        import probatio

        required = probatio.Required
    except ImportError:
        import voluptuous as vol

        required = vol.Required
    from homeassistant.helpers import config_validation as cv

    return {required("query"): cv.string}


class _PlayerIntent(IntentHandler):
    async def _player(self, intent_obj: Intent):
        from . import player

        entity = player(intent_obj.hass)
        if entity is None:
            return None, _speech(intent_obj, "Ютуб не настроен")
        return entity, None


class PlayIntent(_PlayerIntent):
    intent_type = INTENT_PLAY
    description = (
        "Включить ютуб и продолжить очередь, которая уже была. "
        "Если очередь пустая, включить подборку. "
        "Вызывай, когда просят включить, запустить или продолжить музыку ютуба."
    )

    async def async_handle(self, intent_obj: Intent) -> IntentResponse:
        entity, missing = await self._player(intent_obj)
        if missing:
            return missing
        return _speech(intent_obj, await entity.async_play_youtube())


class NextIntent(_PlayerIntent):
    intent_type = INTENT_NEXT
    description = (
        "Следующий трек очереди ютуба. "
        "Вызывай и на неточные фразы: дальше, следующий, ещё песню, переключи, "
        "если речь о ютубе."
    )

    async def async_handle(self, intent_obj: Intent) -> IntentResponse:
        entity, missing = await self._player(intent_obj)
        if missing:
            return missing
        return _speech(intent_obj, await entity.async_voice_next())


class PreviousIntent(_PlayerIntent):
    intent_type = INTENT_PREVIOUS
    description = (
        "Предыдущий трек очереди ютуба. "
        "Вызывай и на неточные фразы: назад, верни, прошлый, предыдущий, "
        "если речь о ютубе."
    )

    async def async_handle(self, intent_obj: Intent) -> IntentResponse:
        entity, missing = await self._player(intent_obj)
        if missing:
            return missing
        return _speech(intent_obj, await entity.async_voice_previous())


class SearchIntent(_PlayerIntent):
    intent_type = INTENT_SEARCH
    description = (
        "Найти на ютубе и сразу включить. "
        "query — исполнитель, песня, альбом или плейлист."
    )
    slot_schema = _search_schema()

    async def async_handle(self, intent_obj: Intent) -> IntentResponse:
        entity, missing = await self._player(intent_obj)
        if missing:
            return missing
        return _speech(intent_obj, await entity.async_voice_search(_query(intent_obj)))


class ClearIntent(_PlayerIntent):
    intent_type = INTENT_CLEAR
    description = (
        "Только очистить очередь ютуба и ничего не включать. "
        "Вызывай, когда просят удалить, очистить или сбросить список ютуба."
    )

    async def async_handle(self, intent_obj: Intent) -> IntentResponse:
        entity, missing = await self._player(intent_obj)
        if missing:
            return missing
        return _speech(intent_obj, await entity.async_voice_clear())


class ForwardIntent(_PlayerIntent):
    intent_type = INTENT_FORWARD
    description = (
        "Перемотать ютуб на 15 секунд вперёд внутри текущего трека, не переключая песню. "
        "Вызывай на «промотай», «перемотай вперёд на пятнадцать», если речь о ютубе."
    )

    async def async_handle(self, intent_obj: Intent) -> IntentResponse:
        entity, missing = await self._player(intent_obj)
        if missing:
            return missing
        return _speech(intent_obj, await entity.async_voice_seek(15000))


class RewindIntent(_PlayerIntent):
    intent_type = INTENT_REWIND
    description = (
        "Перемотать ютуб на 15 секунд назад внутри текущего трека, не включая предыдущую песню. "
        "Вызывай на «отмотай», «верни на пятнадцать секунд», если речь о ютубе."
    )

    async def async_handle(self, intent_obj: Intent) -> IntentResponse:
        entity, missing = await self._player(intent_obj)
        if missing:
            return missing
        return _speech(intent_obj, await entity.async_voice_seek(-15000))


class RandomIntent(_PlayerIntent):
    intent_type = INTENT_RANDOM
    description = (
        "Очистить очередь ютуба и включить случайные треки. "
        "Вызывай, когда просят случайное, рандом или что-нибудь на ютубе."
    )

    async def async_handle(self, intent_obj: Intent) -> IntentResponse:
        entity, missing = await self._player(intent_obj)
        if missing:
            return missing
        return _speech(intent_obj, await entity.async_voice_random())


class SpeakerOnIntent(_PlayerIntent):
    intent_type = INTENT_SPEAKER_ON
    description = (
        "Включить встроенный динамик колонки, оба канала усилителя. "
        "Работает только на Яндекс Станции Лайт 2. "
        "Это не пауза и не громкость ютуба: звук снова идёт из колонки."
    )

    async def async_handle(self, intent_obj: Intent) -> IntentResponse:
        entity, missing = await self._player(intent_obj)
        if missing:
            return missing
        return _speech(intent_obj, await entity.async_voice_speaker(True))


class SpeakerOffIntent(_PlayerIntent):
    intent_type = INTENT_SPEAKER_OFF
    description = (
        "Выключить встроенный динамик колонки, оба канала усилителя. "
        "Работает только на Яндекс Станции Лайт 2. "
        "Музыка и ответы замолкают, очередь ютуба не останавливается. "
        "Это не пауза и не «тише»."
    )

    async def async_handle(self, intent_obj: Intent) -> IntentResponse:
        entity, missing = await self._player(intent_obj)
        if missing:
            return missing
        return _speech(intent_obj, await entity.async_voice_speaker(False))


HANDLERS = (
    PlayIntent,
    NextIntent,
    PreviousIntent,
    SearchIntent,
    ClearIntent,
    RandomIntent,
    ForwardIntent,
    RewindIntent,
    SpeakerOnIntent,
    SpeakerOffIntent,
)
