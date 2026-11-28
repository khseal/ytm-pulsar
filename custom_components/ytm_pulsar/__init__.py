"""YouTube Music on a Pulsar speaker, controlled from Home Assistant."""

from __future__ import annotations

import logging
from pathlib import Path

from homeassistant.config_entries import ConfigEntry
from homeassistant.const import Platform
from homeassistant.core import HomeAssistant
from homeassistant.exceptions import ConfigEntryNotReady, HomeAssistantError

from .api import YtmClient, YtmError
from .const import CONF_URL, DOMAIN, SENTENCE_FILE, SENTENCE_MARKER
from .voice import register, unregister

_LOGGER = logging.getLogger(__name__)
PLATFORMS = [Platform.MEDIA_PLAYER, Platform.SWITCH]


async def async_setup_entry(hass: HomeAssistant, entry: ConfigEntry) -> bool:
    """Connect to one station."""
    client = YtmClient(hass, entry.data[CONF_URL])
    try:
        await client.info()
    except YtmError as err:
        raise ConfigEntryNotReady(str(err)) from err

    store = hass.data.setdefault(DOMAIN, {"entries": {}})
    store["entries"][entry.entry_id] = {"client": client}
    if not store.get("voice"):
        register(hass)
        await _install_sentences(hass)
        store["voice"] = True

    await hass.config_entries.async_forward_entry_setups(entry, PLATFORMS)
    return True


async def async_unload_entry(hass: HomeAssistant, entry: ConfigEntry) -> bool:
    """Drop one station. Voice phrases stay while any station remains."""
    unloaded = await hass.config_entries.async_unload_platforms(entry, PLATFORMS)
    if not unloaded:
        return False
    store = hass.data.get(DOMAIN) or {}
    store.get("entries", {}).pop(entry.entry_id, None)
    if store.get("entries"):
        return True
    unregister(hass)
    await _remove_sentences(hass)
    hass.data.pop(DOMAIN, None)
    return True


def player(hass: HomeAssistant):
    """The media player voice commands should drive."""
    for item in hass.data.get(DOMAIN, {}).get("entries", {}).values():
        entity = item.get("player")
        if entity is not None:
            return entity
    return None


async def _install_sentences(hass: HomeAssistant) -> None:
    source = Path(__file__).parent / "custom_sentences" / "ru" / SENTENCE_FILE
    destination = Path(hass.config.path("custom_sentences", "ru", SENTENCE_FILE))
    text = source.read_text(encoding="utf-8")

    def write() -> bool:
        destination.parent.mkdir(parents=True, exist_ok=True)
        if destination.is_file() and destination.read_text(encoding="utf-8") == text:
            return False
        destination.write_text(text, encoding="utf-8")
        return True

    changed = await hass.async_add_executor_job(write)
    if not changed:
        return
    await _reload_conversation(hass)


async def _remove_sentences(hass: HomeAssistant) -> None:
    destination = Path(hass.config.path("custom_sentences", "ru", SENTENCE_FILE))

    def remove() -> bool:
        if not destination.is_file():
            return False
        text = destination.read_text(encoding="utf-8")
        if SENTENCE_MARKER not in text:
            return False
        destination.unlink()
        return True

    if await hass.async_add_executor_job(remove):
        await _reload_conversation(hass)


async def _reload_conversation(hass: HomeAssistant) -> None:
    if not hass.services.has_service("conversation", "reload"):
        _LOGGER.info(
            "Фразы YouTube записаны. Перезапустите Home Assistant, чтобы ассистент их услышал"
        )
        return
    try:
        await hass.services.async_call("conversation", "reload", blocking=True)
    except HomeAssistantError:
        _LOGGER.info(
            "Фразы YouTube записаны. Перезапустите Home Assistant, чтобы ассистент их услышал"
        )
