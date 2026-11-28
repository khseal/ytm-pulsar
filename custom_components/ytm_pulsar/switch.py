"""Switch for the station's built-in amplifier."""

from __future__ import annotations

import logging
from datetime import timedelta

from homeassistant.components.switch import SwitchEntity
from homeassistant.config_entries import ConfigEntry
from homeassistant.core import HomeAssistant
from homeassistant.helpers.device_registry import DeviceInfo
from homeassistant.helpers.update_coordinator import (
    CoordinatorEntity,
    DataUpdateCoordinator,
    UpdateFailed,
)

from .api import YtmClient, YtmError
from .const import CONF_URL, DOMAIN

_LOGGER = logging.getLogger(__name__)
SCAN_INTERVAL = timedelta(seconds=10)


async def async_setup_entry(
    hass: HomeAssistant, entry: ConfigEntry, async_add_entities
) -> None:
    """Create the amplifier switch."""
    client: YtmClient = hass.data[DOMAIN]["entries"][entry.entry_id]["client"]

    async def update():
        try:
            return await client.state()
        except YtmError as err:
            raise UpdateFailed(str(err)) from err

    coordinator = DataUpdateCoordinator(
        hass,
        _LOGGER,
        name=f"{DOMAIN}-speaker",
        update_method=update,
        update_interval=SCAN_INTERVAL,
    )
    await coordinator.async_config_entry_first_refresh()
    async_add_entities([YtmSpeakerSwitch(coordinator, client, entry)])


class YtmSpeakerSwitch(CoordinatorEntity, SwitchEntity):
    """Both channels of the FS2105 amplifier."""

    _attr_name = "Встроенный динамик"
    _attr_icon = "mdi:speaker"

    def __init__(self, coordinator, client: YtmClient, entry: ConfigEntry) -> None:
        super().__init__(coordinator)
        self._client = client
        self._attr_unique_id = f"{entry.entry_id}-speaker"
        self._attr_device_info = DeviceInfo(
            identifiers={(DOMAIN, entry.entry_id)},
            name="YouTube Music",
            manufacturer="ytm-pulsar",
            model="YouTube Music",
            configuration_url=entry.data[CONF_URL],
        )

    @property
    def available(self) -> bool:
        data = self.coordinator.data or {}
        return bool(data.get("speaker_known")) and super().available

    @property
    def is_on(self) -> bool:
        return bool((self.coordinator.data or {}).get("speaker_on"))

    async def async_turn_on(self, **kwargs) -> None:
        await self._client.command("speaker-on")
        await self.coordinator.async_request_refresh()

    async def async_turn_off(self, **kwargs) -> None:
        await self._client.command("speaker-off")
        await self.coordinator.async_request_refresh()
