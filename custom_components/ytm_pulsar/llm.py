"""Expose every YouTube command to the Assist language model.

Exact phrases still work. The model may also call these tools when the
wording is different. The reply must say «ютуб»: the speaker's TTS does not
pronounce the English word.
"""

from __future__ import annotations

from homeassistant.core import HomeAssistant, callback
from homeassistant.helpers.llm import LLM_API_ASSIST, IntentTool

from .const import DOMAIN
from .voice import HANDLERS

PROMPT = (
    "Команды ytm_pulsar управляют очередью ютуба на колонке. "
    "Это не Music Assistant и не обычные кнопки колонки. "
    "Включить ютуб значит продолжить уже существующую очередь. "
    "Следующий и предыдущий трек ютуба вызывай, даже если фраза неточная. "
    "Поиск сразу включает найденное. "
    "Очистка только удаляет очередь. "
    "Случайные треки сначала очищают очередь, потом включают новое. "
    "Промотать и отмотать сдвигают текущий трек на 15 секунд и не переключают песню. "
    "Выключить динамик глушит встроенный усилитель и не ставит ютуб на паузу. "
    "Это работает только на Яндекс Станции Лайт 2. "
    "Включить динамик снова открывает оба канала усилителя. "
    "Отвечай по-русски и пиши слово ютуб кириллицей."
)


def _tool(handler):
    """Prefix the tool name on current Home Assistant and keep the intent type."""
    prefixed = f"{DOMAIN}__{handler.intent_type}"
    try:
        tool = IntentTool(prefixed, handler, integration=DOMAIN)
    except TypeError:
        tool = IntentTool(prefixed, handler)
    if not hasattr(tool, "intent_type"):
        try:
            tool = IntentTool(handler.intent_type, handler, integration=DOMAIN)
        except TypeError:
            tool = IntentTool(handler.intent_type, handler)
    return tool


@callback
def async_get_tools(hass: HomeAssistant, llm_context, api_id: str):
    """Give Assist the YouTube commands."""
    if api_id != LLM_API_ASSIST:
        return None
    from homeassistant.components.llm import LLMTools

    return LLMTools(tools=[_tool(handler()) for handler in HANDLERS], prompt=PROMPT)
