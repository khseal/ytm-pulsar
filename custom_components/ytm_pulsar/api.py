"""HTTP client for the ytm-pulsar panel."""

from __future__ import annotations

import asyncio

import aiohttp

from homeassistant.helpers.aiohttp_client import async_get_clientsession

COMMAND_TIMEOUT = 50


class YtmError(Exception):
    """The panel refused a command or could not be reached."""


class YtmClient:
    """Talks to the station panel.

    A command is accepted only when Origin is the panel's own address and the
    request carries the panel client header.
    """

    def __init__(self, hass, url: str) -> None:
        self._hass = hass
        self.url = url.rstrip("/")

    async def info(self) -> dict:
        return await self._request("GET", "/api/info")

    async def state(self) -> dict:
        data = await self._request("GET", "/api/state")
        state = data.get("state")
        return state if isinstance(state, dict) else {}

    async def command(self, action: str, **fields) -> dict:
        body = {"action": action}
        for key, value in fields.items():
            if value is not None:
                body[key] = value
        return await self._request("POST", "/api/command", body)

    async def _request(self, method: str, path: str, body: dict | None = None) -> dict:
        session = async_get_clientsession(self._hass)
        headers = {"Origin": self.url, "X-Ytm-Client": "web"}
        try:
            async with asyncio.timeout(COMMAND_TIMEOUT):
                response = await session.request(
                    method,
                    f"{self.url}{path}",
                    json=body,
                    headers=headers,
                )
                data = await response.json(content_type=None)
        except (TimeoutError, aiohttp.ClientError) as err:
            raise YtmError("колонка не ответила") from err
        if not isinstance(data, dict):
            raise YtmError("колонка ответила не тем форматом")
        if response.status >= 400 or data.get("error"):
            raise YtmError(str(data.get("error") or "команда не выполнена"))
        return data
