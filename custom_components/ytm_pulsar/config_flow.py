"""Config flow for a ytm-pulsar station."""

from __future__ import annotations

import voluptuous as vol

from homeassistant.config_entries import ConfigFlow
from homeassistant.const import CONF_HOST, CONF_PORT

from .api import YtmClient, YtmError
from .const import CONF_URL, DEFAULT_PORT, DOMAIN

DATA_SCHEMA = vol.Schema(
    {
        vol.Required(CONF_HOST): str,
        vol.Required(CONF_PORT, default=DEFAULT_PORT): vol.All(
            vol.Coerce(int), vol.Range(min=1, max=65535)
        ),
    }
)


class YtmConfigFlow(ConfigFlow, domain=DOMAIN):
    """Ask for the station address. The panel has no password."""

    VERSION = 1

    async def async_step_user(self, user_input=None):
        errors = {}
        if user_input is not None:
            host = user_input[CONF_HOST].strip()
            host = host.removeprefix("http://").removeprefix("https://").strip("/")
            port = user_input[CONF_PORT]
            suffix = f":{port}"
            if host.endswith(suffix):
                host = host[: -len(suffix)]
            url = f"http://{host}:{port}"
            try:
                await YtmClient(self.hass, url).info()
            except YtmError:
                errors["base"] = "cannot_connect"
            else:
                await self.async_set_unique_id(url)
                self._abort_if_unique_id_configured()
                return self.async_create_entry(
                    title="YouTube Music",
                    data={CONF_URL: url, CONF_HOST: host, CONF_PORT: port},
                )
        return self.async_show_form(
            step_id="user", data_schema=DATA_SCHEMA, errors=errors
        )
