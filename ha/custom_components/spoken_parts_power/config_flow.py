"""Config flow: one entry. The only option is the local kill switch."""

from __future__ import annotations

from typing import Any

import voluptuous as vol

from homeassistant.config_entries import (
    ConfigEntry,
    ConfigFlow,
    ConfigFlowResult,
    OptionsFlow,
)
from homeassistant.core import callback
from homeassistant.helpers.selector import BooleanSelector

from .const import CONF_ALLOW_SWITCHING, DOMAIN


class PowerConfigFlow(ConfigFlow, domain=DOMAIN):
    VERSION = 1

    async def async_step_user(self, user_input: dict[str, Any] | None = None) -> ConfigFlowResult:
        await self.async_set_unique_id(DOMAIN)
        self._abort_if_unique_id_configured()
        if user_input is not None:
            return self.async_create_entry(
                title="Spoken Parts Power",
                data={},
                options={CONF_ALLOW_SWITCHING: bool(user_input.get(CONF_ALLOW_SWITCHING, True))},
            )
        return self.async_show_form(
            step_id="user",
            data_schema=vol.Schema(
                {vol.Required(CONF_ALLOW_SWITCHING, default=True): BooleanSelector()}
            ),
        )

    @staticmethod
    @callback
    def async_get_options_flow(config_entry: ConfigEntry) -> OptionsFlow:
        return PowerOptionsFlow()


class PowerOptionsFlow(OptionsFlow):
    async def async_step_init(self, user_input: dict[str, Any] | None = None) -> ConfigFlowResult:
        if user_input is not None:
            return self.async_create_entry(
                data={CONF_ALLOW_SWITCHING: bool(user_input.get(CONF_ALLOW_SWITCHING, True))}
            )
        current = self.config_entry.options.get(CONF_ALLOW_SWITCHING, True)
        return self.async_show_form(
            step_id="init",
            data_schema=vol.Schema(
                {vol.Required(CONF_ALLOW_SWITCHING, default=current): BooleanSelector()}
            ),
        )
