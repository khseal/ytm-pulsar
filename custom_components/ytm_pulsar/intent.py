"""Intent platform.

Home Assistant loads this module by itself and calls async_setup_intents.
The handlers live in voice.py so this name does not hide homeassistant.helpers.intent.
"""

from homeassistant.core import HomeAssistant

from .voice import register


async def async_setup_intents(hass: HomeAssistant) -> None:
    """Register the YouTube phrases when Home Assistant loads intent platforms."""
    register(hass)
