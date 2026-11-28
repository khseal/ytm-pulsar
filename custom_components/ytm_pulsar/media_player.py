"""Media player for the YouTube queue on the speaker."""

from __future__ import annotations

import logging
from datetime import timedelta

import voluptuous as vol

from homeassistant.components.media_player import (
    MediaPlayerDeviceClass,
    MediaPlayerEntity,
    MediaPlayerEntityFeature,
    MediaPlayerState,
    MediaType,
)
from homeassistant.config_entries import ConfigEntry
from homeassistant.core import HomeAssistant
from homeassistant.exceptions import HomeAssistantError
from homeassistant.helpers import config_validation as cv, entity_platform
from homeassistant.helpers.device_registry import DeviceInfo
from homeassistant.helpers.update_coordinator import (
    CoordinatorEntity,
    DataUpdateCoordinator,
    UpdateFailed,
)
from homeassistant.util import dt as dt_util

from .api import YtmClient, YtmError
from .const import CONF_URL, DOMAIN

_LOGGER = logging.getLogger(__name__)
SCAN_INTERVAL = timedelta(seconds=10)

FEATURES = (
    MediaPlayerEntityFeature.TURN_ON
    | MediaPlayerEntityFeature.TURN_OFF
    | MediaPlayerEntityFeature.PLAY
    | MediaPlayerEntityFeature.PAUSE
    | MediaPlayerEntityFeature.STOP
    | MediaPlayerEntityFeature.NEXT_TRACK
    | MediaPlayerEntityFeature.PREVIOUS_TRACK
    | MediaPlayerEntityFeature.PLAY_MEDIA
    | MediaPlayerEntityFeature.SEEK
    | MediaPlayerEntityFeature.VOLUME_SET
    | MediaPlayerEntityFeature.SHUFFLE_SET
    | MediaPlayerEntityFeature.CLEAR_PLAYLIST
)


async def async_setup_entry(
    hass: HomeAssistant, entry: ConfigEntry, async_add_entities
) -> None:
    """Create the player and the search/random actions."""
    client: YtmClient = hass.data[DOMAIN]["entries"][entry.entry_id]["client"]

    async def update():
        try:
            return await client.state()
        except YtmError as err:
            raise UpdateFailed(str(err)) from err

    coordinator = DataUpdateCoordinator(
        hass,
        _LOGGER,
        name=DOMAIN,
        update_method=update,
        update_interval=SCAN_INTERVAL,
    )
    await coordinator.async_config_entry_first_refresh()
    async_add_entities([YtmMediaPlayer(coordinator, client, entry)])

    platform = entity_platform.async_get_current_platform()
    platform.async_register_entity_service(
        "search",
        {vol.Required("query"): cv.string},
        "async_search",
    )
    platform.async_register_entity_service("random", {}, "async_random")


class YtmMediaPlayer(CoordinatorEntity, MediaPlayerEntity):
    """One YouTube queue."""

    _attr_device_class = MediaPlayerDeviceClass.SPEAKER
    _attr_icon = "mdi:youtube"
    _attr_name = "YouTube Music"
    _attr_supported_features = FEATURES

    def __init__(self, coordinator, client: YtmClient, entry: ConfigEntry) -> None:
        super().__init__(coordinator)
        self._client = client
        self._entry_id = entry.entry_id
        self._attr_unique_id = entry.entry_id
        self._attr_device_info = DeviceInfo(
            identifiers={(DOMAIN, entry.entry_id)},
            name="YouTube Music",
            manufacturer="ytm-pulsar",
            model="YouTube Music",
            configuration_url=entry.data[CONF_URL],
        )

    async def async_added_to_hass(self) -> None:
        await super().async_added_to_hass()
        self.hass.data[DOMAIN]["entries"][self._entry_id]["player"] = self

    @property
    def _data(self) -> dict:
        return self.coordinator.data or {}

    @property
    def _track(self) -> dict | None:
        queue = self._data.get("queue") or []
        index = self._data.get("index") or 0
        if not isinstance(queue, list) or index < 0 or index >= len(queue):
            return None
        track = queue[index]
        return track if isinstance(track, dict) else None

    @property
    def state(self) -> MediaPlayerState:
        status = self._data.get("status")
        if status in ("playing", "searching"):
            return MediaPlayerState.PLAYING
        if status == "buffering":
            return MediaPlayerState.BUFFERING
        if status == "paused":
            return MediaPlayerState.PAUSED
        return MediaPlayerState.IDLE

    @property
    def media_title(self) -> str | None:
        track = self._track
        return track.get("title") if track else None

    @property
    def media_artist(self) -> str | None:
        track = self._track
        return track.get("artists") if track else None

    @property
    def media_image_url(self) -> str | None:
        track = self._track
        url = track.get("thumbnail_url") if track else None
        return url or None

    @property
    def media_duration(self) -> int | None:
        track = self._track
        if not track or not track.get("duration_ms"):
            return None
        return int(track["duration_ms"] / 1000)

    @property
    def media_position(self) -> int | None:
        if "position_ms" not in self._data:
            return None
        return int(self._data.get("position_ms") or 0) // 1000

    @property
    def media_position_updated_at(self):
        return dt_util.utcnow()

    @property
    def volume_level(self) -> float | None:
        if not self._data.get("has_volume"):
            return None
        return max(0.0, min(1.0, (self._data.get("volume") or 0) / 100))

    @property
    def shuffle(self) -> bool:
        return bool(self._data.get("shuffle"))

    @property
    def extra_state_attributes(self) -> dict:
        queue = self._data.get("queue") or []
        names = []
        if isinstance(queue, list):
            for track in queue[:40]:
                if isinstance(track, dict):
                    names.append(f"{track.get('artists') or ''} — {track.get('title') or ''}".strip(" —"))
        attributes = {"queue_size": len(queue) if isinstance(queue, list) else 0, "queue": names}
        if self._data.get("error"):
            attributes["error"] = self._data["error"]
        return attributes

    async def _run(self, action: str, **fields) -> None:
        try:
            await self._client.command(action, **fields)
        except YtmError as err:
            raise HomeAssistantError(str(err)) from err
        await self.coordinator.async_request_refresh()

    async def _say(self, action) -> str:
        try:
            text = await action()
        except YtmError as err:
            text = str(err)
        await self.coordinator.async_request_refresh()
        return text

    async def async_turn_on(self) -> None:
        await self.async_play_youtube()

    async def async_turn_off(self) -> None:
        await self._run("stop")

    async def async_media_play(self) -> None:
        await self._run("resume")

    async def async_media_pause(self) -> None:
        await self._run("pause")

    async def async_media_stop(self) -> None:
        await self._run("stop")

    async def async_media_next_track(self) -> None:
        await self._run("next")

    async def async_media_previous_track(self) -> None:
        await self._run("previous")

    async def async_play_media(self, media_type: MediaType | str, media_id: str, **kwargs) -> None:
        await self.async_search(media_id)

    async def async_media_seek(self, position: float) -> None:
        await self._run("seek", position_ms=int(position * 1000))

    async def async_set_volume_level(self, volume: float) -> None:
        await self._run("volume", volume=int(round(volume * 100)))

    async def async_set_shuffle(self, shuffle: bool) -> None:
        await self._run("shuffle-on" if shuffle else "shuffle-off")

    async def async_clear_playlist(self) -> None:
        await self._run("clear")

    async def async_search(self, query: str) -> None:
        text = await self.async_voice_search(query)
        if not text.startswith("Ищу"):
            raise HomeAssistantError(text)

    async def async_random(self) -> None:
        text = await self.async_voice_random()
        if not text.startswith("Включаю"):
            raise HomeAssistantError(text)

    async def async_play_youtube(self) -> str:
        async def play():
            state = await self._client.state()
            if state.get("queue"):
                if state.get("status") == "playing":
                    return "Ютуб уже играет"
                await self._client.command("resume")
                return "Включаю ютуб"
            try:
                await self._client.command("recommendations")
                return "Включаю подборку ютуба"
            except YtmError:
                await self._client.command("surprise")
                return "Включаю случайные треки с ютуба"

        return await self._say(play)

    async def async_voice_next(self) -> str:
        async def play():
            await self._client.command("next")
            return "Дальше на ютубе"

        return await self._say(play)

    async def async_voice_previous(self) -> str:
        async def play():
            await self._client.command("previous")
            return "Назад на ютубе"

        return await self._say(play)

    async def async_voice_search(self, query: str) -> str:
        query = query.strip()
        if not query:
            return "Что включить на ютубе?"

        async def play():
            await self._client.command("play", query=query)
            return f"Ищу на ютубе: {query}"

        return await self._say(play)

    async def async_voice_clear(self) -> str:
        async def play():
            await self._client.command("clear")
            return "Список ютуба очищен"

        return await self._say(play)

    async def async_voice_seek(self, delta_ms: int) -> str:
        async def play():
            await self._client.command("seek-by", position_ms=delta_ms)
            if delta_ms < 0:
                return "Отматываю ютуб на пятнадцать секунд"
            return "Проматываю ютуб на пятнадцать секунд"

        return await self._say(play)

    async def async_voice_random(self) -> str:
        async def play():
            await self._client.command("clear")
            await self._client.command("surprise")
            return "Включаю случайные треки с ютуба"

        return await self._say(play)

    async def async_voice_speaker(self, on: bool) -> str:
        async def play():
            await self._client.command("speaker-on" if on else "speaker-off")
            return "Включаю динамик" if on else "Выключаю динамик"

        return await self._say(play)
