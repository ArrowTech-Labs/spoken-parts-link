"""Config flow: one entry per printer."""

from __future__ import annotations

import re
from typing import Any

import voluptuous as vol

from homeassistant.components.bluetooth import BluetoothServiceInfoBleak
from homeassistant.config_entries import (
    ConfigEntry,
    ConfigFlow,
    ConfigFlowResult,
    OptionsFlow,
)
from homeassistant.core import callback
from homeassistant.helpers.selector import (
    BooleanSelector,
    NumberSelector,
    NumberSelectorConfig,
    NumberSelectorMode,
    SelectOptionDict,
    SelectSelector,
    SelectSelectorConfig,
    SelectSelectorMode,
    TextSelector,
)

from .const import (
    CONF_ADDRESS,
    CONF_DRIVER,
    CONF_KEEP_CONNECTED,
    CONF_MEDIA,
    CONF_NAME,
    CONF_RFCOMM_CHANNEL,
    CONF_TRANSPORT,
    DOMAIN,
)
from .drivers import registry
from .drivers.base import Driver, Media

ADDRESS_RE = re.compile(r"^[0-9A-F]{2}(:[0-9A-F]{2}){5}$")

TRANSPORT_LABELS = {
    "spp": "Bluetooth Classic (serial port)",
    "ble": "Bluetooth LE",
    "usb": "USB",
    "ipp": "Network (IPP)",
    "fake": "Test only",
}
KIND_LABELS = {"die_cut": "labels", "continuous": "continuous roll", "shipping_4x6": "shipping labels"}

# Best effort: the P21 advertises over LE only while no Classic link is up.
DISCOVERY_PREFIXES = {"P21": "nelko.p21"}


def unique_id_for(driver_id: str, address: str) -> str:
    return f"{driver_id}:{address.lower()}"


def _media_label(m: Media) -> str:
    size = f"{m.width_mm:g} x {m.length_mm:g} mm" if m.length_mm else f"{m.width_mm:g} mm"
    return f"{size} {KIND_LABELS.get(m.kind, m.kind)}"


def _transport_selector(driver: Driver) -> SelectSelector:
    return SelectSelector(
        SelectSelectorConfig(
            options=[
                SelectOptionDict(value=t, label=TRANSPORT_LABELS.get(t, t))
                for t in driver.capabilities.transports
            ],
            mode=SelectSelectorMode.DROPDOWN,
        )
    )


def _media_selector(driver: Driver) -> SelectSelector:
    return SelectSelector(
        SelectSelectorConfig(
            options=[
                SelectOptionDict(value=m.id, label=_media_label(m))
                for m in driver.capabilities.media
            ],
            mode=SelectSelectorMode.DROPDOWN,
            custom_value=True,
        )
    )


def _check(driver: Driver, data: dict[str, Any]) -> dict[str, str]:
    errors: dict[str, str] = {}
    if CONF_TRANSPORT in data and data[CONF_TRANSPORT] not in driver.capabilities.transports:
        errors[CONF_TRANSPORT] = "bad_transport"
    if CONF_MEDIA in data and driver.media_for(data[CONF_MEDIA]) is None:
        errors[CONF_MEDIA] = "bad_media"
    return errors


class SpokenPartsPrintersConfigFlow(ConfigFlow, domain=DOMAIN):
    VERSION = 1

    def __init__(self) -> None:
        self._driver_id: str | None = None
        self._address: str | None = None
        self._name: str | None = None

    @staticmethod
    @callback
    def async_get_options_flow(config_entry: ConfigEntry) -> OptionsFlow:
        return SpokenPartsPrintersOptionsFlow()

    async def async_step_user(self, user_input: dict[str, Any] | None = None) -> ConfigFlowResult:
        choices = registry.choices()
        if not choices:
            return self.async_abort(reason="no_drivers")
        if user_input is not None and user_input.get(CONF_DRIVER) in choices:
            self._driver_id = user_input[CONF_DRIVER]
            return await self.async_step_printer()
        schema = vol.Schema(
            {
                vol.Required(CONF_DRIVER): SelectSelector(
                    SelectSelectorConfig(
                        options=[SelectOptionDict(value=k, label=v) for k, v in choices.items()],
                        mode=SelectSelectorMode.DROPDOWN,
                    )
                )
            }
        )
        return self.async_show_form(step_id="user", data_schema=schema)

    async def async_step_bluetooth(
        self, discovery_info: BluetoothServiceInfoBleak
    ) -> ConfigFlowResult:
        """Best effort discovery from BLE advertisements (local name P21...)."""
        name = discovery_info.name or ""
        driver_id = next(
            (d for p, d in DISCOVERY_PREFIXES.items() if name.upper().startswith(p)), None
        )
        if driver_id is None or registry.get(driver_id) is None:
            return self.async_abort(reason="not_supported")
        address = discovery_info.address.upper()
        await self.async_set_unique_id(unique_id_for(driver_id, address))
        self._abort_if_unique_id_configured()
        self._driver_id, self._address, self._name = driver_id, address, name
        self.context["title_placeholders"] = {"name": name}
        return await self.async_step_printer()

    async def async_step_printer(self, user_input: dict[str, Any] | None = None) -> ConfigFlowResult:
        driver = registry.get(self._driver_id or "")
        if driver is None:
            return self.async_abort(reason="no_drivers")
        caps = driver.capabilities
        errors: dict[str, str] = {}
        if user_input is not None:
            address = str(user_input[CONF_ADDRESS]).strip().upper().replace("-", ":")
            errors = _check(driver, user_input)
            if not ADDRESS_RE.match(address):
                errors[CONF_ADDRESS] = "invalid_address"
            if not errors:
                await self.async_set_unique_id(unique_id_for(driver.id, address))
                self._abort_if_unique_id_configured()
                name = str(user_input.get(CONF_NAME) or f"{driver.vendor} {driver.model}").strip()
                return self.async_create_entry(
                    title=name,
                    data={
                        CONF_DRIVER: driver.id,
                        CONF_ADDRESS: address,
                        CONF_NAME: name,
                        CONF_TRANSPORT: user_input[CONF_TRANSPORT],
                        CONF_MEDIA: user_input[CONF_MEDIA],
                    },
                )
        defaults = user_input or {}
        schema = vol.Schema(
            {
                vol.Required(
                    CONF_ADDRESS, default=defaults.get(CONF_ADDRESS, self._address or "")
                ): TextSelector(),
                vol.Required(
                    CONF_NAME,
                    default=defaults.get(CONF_NAME, f"{driver.vendor} {driver.model}"),
                ): TextSelector(),
                vol.Required(
                    CONF_TRANSPORT, default=defaults.get(CONF_TRANSPORT, caps.transports[0])
                ): _transport_selector(driver),
                vol.Required(
                    CONF_MEDIA, default=defaults.get(CONF_MEDIA, caps.default_media)
                ): _media_selector(driver),
            }
        )
        return self.async_show_form(
            step_id="printer",
            data_schema=schema,
            errors=errors,
            description_placeholders={"model": f"{driver.vendor} {driver.model}"},
        )


class SpokenPartsPrintersOptionsFlow(OptionsFlow):
    """Change the name, transport, loaded labels and connection options."""

    async def async_step_init(self, user_input: dict[str, Any] | None = None) -> ConfigFlowResult:
        entry = self.config_entry
        driver = registry.get(entry.data[CONF_DRIVER])
        if driver is None:
            return self.async_abort(reason="no_drivers")
        current = {**entry.data, **entry.options}
        errors: dict[str, str] = {}
        if user_input is not None:
            errors = _check(driver, user_input)
            if not errors:
                data = dict(user_input)
                if CONF_RFCOMM_CHANNEL in data:
                    data[CONF_RFCOMM_CHANNEL] = int(data[CONF_RFCOMM_CHANNEL])
                return self.async_create_entry(data=data)
            current.update(user_input)
        transport = current.get(CONF_TRANSPORT, driver.capabilities.transports[0])
        fields: dict[Any, Any] = {
            vol.Required(CONF_NAME, default=current.get(CONF_NAME, entry.title)): TextSelector(),
            vol.Required(CONF_TRANSPORT, default=transport): _transport_selector(driver),
            vol.Required(
                CONF_MEDIA, default=current.get(CONF_MEDIA, driver.capabilities.default_media)
            ): _media_selector(driver),
            vol.Required(
                CONF_KEEP_CONNECTED,
                default=current.get(CONF_KEEP_CONNECTED, transport == "spp"),
            ): BooleanSelector(),
        }
        if driver.spp is not None:
            fields[
                vol.Required(
                    CONF_RFCOMM_CHANNEL,
                    default=current.get(CONF_RFCOMM_CHANNEL, driver.spp.channel),
                )
            ] = NumberSelector(
                NumberSelectorConfig(min=1, max=30, step=1, mode=NumberSelectorMode.BOX)
            )
        return self.async_show_form(
            step_id="init", data_schema=vol.Schema(fields), errors=errors
        )
