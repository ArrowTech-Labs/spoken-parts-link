"""Status and battery sensors for each printer."""

from __future__ import annotations

from typing import Any

from homeassistant.components.sensor import (
    SensorDeviceClass,
    SensorEntity,
    SensorStateClass,
)
from homeassistant.const import PERCENTAGE, EntityCategory
from homeassistant.core import HomeAssistant
from homeassistant.helpers.device_registry import CONNECTION_BLUETOOTH, DeviceInfo
from homeassistant.helpers.entity_platform import AddConfigEntryEntitiesCallback

from .const import DOMAIN
from .integration import PrintersConfigEntry
from .printer import STATES, PrinterRuntime, iso_z


async def async_setup_entry(
    hass: HomeAssistant,
    entry: PrintersConfigEntry,
    async_add_entities: AddConfigEntryEntitiesCallback,
) -> None:
    runtime = entry.runtime_data
    entities: list[SensorEntity] = [PrinterStatusSensor(runtime)]
    if runtime.driver.capabilities.battery:
        entities.append(PrinterBatterySensor(runtime))
    async_add_entities(entities)


class _PrinterEntity(SensorEntity):
    _attr_has_entity_name = True
    _attr_should_poll = False

    def __init__(self, runtime: PrinterRuntime, key: str) -> None:
        self.runtime = runtime
        self._attr_unique_id = f"{runtime.printer_id}:{key}"
        self._attr_translation_key = key
        self._attr_device_info = DeviceInfo(
            identifiers={(DOMAIN, runtime.printer_id)},
            connections={(CONNECTION_BLUETOOTH, runtime.address)},
            manufacturer=runtime.driver.vendor,
            model=runtime.driver.model,
            name=runtime.name,
        )

    async def async_added_to_hass(self) -> None:
        self.async_on_remove(self.runtime.add_listener(self.async_write_ha_state))


class PrinterStatusSensor(_PrinterEntity):
    _attr_device_class = SensorDeviceClass.ENUM
    _attr_options = list(STATES)

    def __init__(self, runtime: PrinterRuntime) -> None:
        super().__init__(runtime, "status")

    @property
    def native_value(self) -> str:
        return self.runtime.state

    @property
    def extra_state_attributes(self) -> dict[str, Any]:
        r = self.runtime
        return {
            "printer_id": r.printer_id,
            "driver": r.driver.id,
            "transport": r.transport.name,
            "media": r.media_id,
            "last_error": r.last_error,
            "last_error_code": r.last_error_code,
            "last_print": r.last_print,
            "last_seen": iso_z(r.last_seen),
        }


class PrinterBatterySensor(_PrinterEntity):
    _attr_device_class = SensorDeviceClass.BATTERY
    _attr_state_class = SensorStateClass.MEASUREMENT
    _attr_native_unit_of_measurement = PERCENTAGE
    _attr_entity_category = EntityCategory.DIAGNOSTIC

    def __init__(self, runtime: PrinterRuntime) -> None:
        super().__init__(runtime, "battery")

    @property
    def native_value(self) -> int | None:
        return self.runtime.battery

    @property
    def extra_state_attributes(self) -> dict[str, Any]:
        return {"charging": self.runtime.charging}
