"""Home Assistant setup and services for Spoken Parts Printers."""

from __future__ import annotations

import asyncio
import logging
from datetime import timedelta
from typing import Any

import voluptuous as vol

from homeassistant.config_entries import ConfigEntry
from homeassistant.const import Platform
from homeassistant.core import (
    HomeAssistant,
    ServiceCall,
    ServiceResponse,
    SupportsResponse,
    callback,
)
from homeassistant.exceptions import ConfigEntryError
from homeassistant.helpers import config_validation as cv
from homeassistant.helpers.event import async_track_time_interval
from homeassistant.helpers.typing import ConfigType

from .const import (
    CONF_ADDRESS,
    CONF_DRIVER,
    CONF_KEEP_CONNECTED,
    CONF_MEDIA,
    CONF_NAME,
    CONF_RFCOMM_CHANNEL,
    CONF_TRANSPORT,
    DOMAIN,
    REFRESH_INTERVAL_S,
    SERVICE_LIST_PRINTERS,
    SERVICE_PRINT_RASTER,
    SERVICE_TIMEOUT_S,
    VERSION,
)
from .drivers import registry
from .drivers.base import PRINTER_ERROR, TIMEOUT, UNKNOWN_PRINTER, Driver
from .printer import PrinterRuntime, fail
from .transports.base import Transport
from .transports.ble import BleTransport
from .transports.fake import FakeTransport
from .transports.rfcomm import RfcommTransport

_LOGGER = logging.getLogger(__name__)

PLATFORMS = [Platform.SENSOR]
CONFIG_SCHEMA = cv.config_entry_only_config_schema(DOMAIN)

type PrintersConfigEntry = ConfigEntry[PrinterRuntime]

_MEDIA_SCHEMA = vol.Schema(
    {
        vol.Required("id"): cv.string,
        vol.Optional("kind"): cv.string,
        vol.Optional("width_mm"): vol.Any(None, vol.Coerce(float)),
        vol.Optional("length_mm"): vol.Any(None, vol.Coerce(float)),
    },
    extra=vol.ALLOW_EXTRA,
)
_PAGE_SCHEMA = vol.Schema(
    {
        vol.Required("width"): vol.All(int, vol.Range(min=1)),
        vol.Required("height"): vol.All(int, vol.Range(min=1)),
        vol.Required("data_b64"): str,
    },
    extra=vol.ALLOW_EXTRA,
)
PRINT_RASTER_SCHEMA = vol.Schema(
    {
        vol.Required("printer_id"): cv.string,
        vol.Required("job_id"): cv.string,
        vol.Required("driver"): cv.string,
        vol.Optional("document_type", default=""): cv.string,
        # The link omits media when a job has none: the loaded labels are used.
        vol.Optional("media"): vol.Any(None, _MEDIA_SCHEMA),
        vol.Optional("copies", default=1): vol.All(int, vol.Range(min=1)),
        vol.Required("pages"): vol.All([_PAGE_SCHEMA], vol.Length(min=1)),
        # Not in the service field list of the contract, but part of the job
        # payload; accepted so the link can pass the payload through as is.
        vol.Optional("format", default="raster"): cv.string,
        vol.Optional("id"): cv.string,
    }
)


def _runtimes(hass: HomeAssistant) -> list[PrinterRuntime]:
    return [
        entry.runtime_data
        for entry in hass.config_entries.async_loaded_entries(DOMAIN)
        if isinstance(getattr(entry, "runtime_data", None), PrinterRuntime)
    ]


async def async_setup(hass: HomeAssistant, config: ConfigType) -> bool:
    """Register the two services once for the domain."""

    async def list_printers(call: ServiceCall) -> ServiceResponse:
        return {"version": VERSION, "printers": [r.as_dict() for r in _runtimes(hass)]}

    async def print_raster(call: ServiceCall) -> ServiceResponse:
        data: dict[str, Any] = dict(call.data)
        runtime = next(
            (r for r in _runtimes(hass) if r.printer_id == data["printer_id"]), None
        )
        if runtime is None:
            return fail(UNKNOWN_PRINTER, "No printer with this id is set up in Home Assistant")
        try:
            async with asyncio.timeout(SERVICE_TIMEOUT_S):
                return await runtime.print_raster(data)
        except TimeoutError:
            return fail(TIMEOUT, "The printer did not finish the job in time")
        except Exception:  # noqa: BLE001 (the service never raises for printer problems)
            _LOGGER.exception("Unexpected error while printing on %s", runtime.name)
            return fail(PRINTER_ERROR, "Unexpected error while printing, see the Home Assistant log")

    hass.services.async_register(
        DOMAIN,
        SERVICE_LIST_PRINTERS,
        list_printers,
        schema=vol.Schema({}),
        supports_response=SupportsResponse.ONLY,
    )
    hass.services.async_register(
        DOMAIN,
        SERVICE_PRINT_RASTER,
        print_raster,
        schema=PRINT_RASTER_SCHEMA,
        supports_response=SupportsResponse.ONLY,
    )
    return True


def build_transport(
    hass: HomeAssistant, driver: Driver, settings: dict[str, Any]
) -> Transport:
    """Make the transport an entry asks for. Tests patch this to use the fake."""
    name = settings[CONF_TRANSPORT]
    address = settings[CONF_ADDRESS].upper()
    if name not in driver.capabilities.transports:
        raise ConfigEntryError(f"The {driver.id} driver does not support the {name} transport")
    if name == "spp":
        assert driver.spp is not None
        return RfcommTransport(
            address,
            driver.spp,
            channel=settings.get(CONF_RFCOMM_CHANNEL) or None,
            keep_alive=settings.get(CONF_KEEP_CONNECTED, True),
        )
    if name == "ble":
        assert driver.ble is not None
        from homeassistant.components import bluetooth  # noqa: PLC0415

        def device_getter() -> Any:
            return bluetooth.async_ble_device_from_address(hass, address, connectable=True)

        async def connector(device: Any, on_disconnect: Any) -> Any:
            from bleak_retry_connector import (  # noqa: PLC0415 (ships with HA bluetooth)
                BleakClientWithServiceCache,
                establish_connection,
            )

            return await establish_connection(
                BleakClientWithServiceCache,
                device,
                settings.get(CONF_NAME) or address,
                disconnected_callback=on_disconnect,
                ble_device_callback=lambda: device_getter() or device,
                max_attempts=3,
            )

        return BleTransport(
            address,
            driver.ble,
            device_getter=device_getter,
            connector=connector,
            keep_alive=settings.get(CONF_KEEP_CONNECTED, False),
        )
    if name == "fake":
        return FakeTransport()
    raise ConfigEntryError(f"Transport {name} is not supported yet")


async def async_setup_entry(hass: HomeAssistant, entry: PrintersConfigEntry) -> bool:
    settings = {**entry.data, **entry.options}
    driver = registry.get(settings[CONF_DRIVER])
    if driver is None:
        raise ConfigEntryError(f"Unknown printer driver {settings[CONF_DRIVER]}")
    transport = build_transport(hass, driver, settings)
    runtime = PrinterRuntime(
        printer_id=entry.unique_id or entry.entry_id,
        name=settings.get(CONF_NAME) or entry.title,
        driver=driver,
        transport=transport,
        address=settings[CONF_ADDRESS],
        media_id=settings[CONF_MEDIA],
    )
    entry.runtime_data = runtime

    await hass.config_entries.async_forward_entry_setups(entry, PLATFORMS)

    @callback
    def _refresh(_now: Any = None) -> None:
        entry.async_create_background_task(
            hass, runtime.refresh(), f"{DOMAIN} refresh {runtime.printer_id}"
        )

    entry.async_on_unload(
        async_track_time_interval(
            hass, _refresh, timedelta(seconds=REFRESH_INTERVAL_S), name=f"{DOMAIN} refresh"
        )
    )
    entry.async_on_unload(entry.add_update_listener(_async_options_updated))
    _refresh()
    return True


async def _async_options_updated(hass: HomeAssistant, entry: PrintersConfigEntry) -> None:
    name = entry.options.get(CONF_NAME)
    if name and name != entry.title:
        # Changing the title calls this listener again, which then reloads.
        hass.config_entries.async_update_entry(entry, title=name)
        return
    await hass.config_entries.async_reload(entry.entry_id)


async def async_unload_entry(hass: HomeAssistant, entry: PrintersConfigEntry) -> bool:
    ok = await hass.config_entries.async_unload_platforms(entry, PLATFORMS)
    if ok:
        await entry.runtime_data.close()
    return ok
