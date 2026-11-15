"""Per circuit: the live sum the engine holds, and its state."""

from __future__ import annotations

import time
from typing import Any

from homeassistant.components.sensor import (
    SensorDeviceClass,
    SensorEntity,
    SensorStateClass,
)
from homeassistant.const import UnitOfPower
from homeassistant.core import HomeAssistant, callback
from homeassistant.helpers.dispatcher import async_dispatcher_connect
from homeassistant.helpers.entity_platform import AddConfigEntryEntitiesCallback

from .const import SIGNAL_UPDATE
from .engine.core import STATES
from .integration import PowerConfigEntry, PowerRuntime


async def async_setup_entry(
    hass: HomeAssistant,
    entry: PowerConfigEntry,
    async_add_entities: AddConfigEntryEntitiesCallback,
) -> None:
    rt = entry.runtime_data
    known: set[str] = set()

    @callback
    def add_new() -> None:
        new: list[SensorEntity] = []
        for cid, c in rt.engine.circuits.items():
            if cid in known:
                continue
            known.add(cid)
            new += [CircuitSum(rt, cid, c.cfg.name), CircuitState(rt, cid, c.cfg.name)]
        if new:
            async_add_entities(new)

    add_new()
    entry.async_on_unload(async_dispatcher_connect(hass, SIGNAL_UPDATE, add_new))


class _CircuitEntity(SensorEntity):
    _attr_should_poll = False

    def __init__(self, rt: PowerRuntime, cid: str, name: str, key: str) -> None:
        self.rt, self.cid = rt, cid
        self._attr_unique_id = f"{cid}:{key}"
        self._attr_name = f"SP power {name} {key}"

    @property
    def available(self) -> bool:
        return self.cid in self.rt.engine.circuits

    async def async_added_to_hass(self) -> None:
        self.async_on_remove(
            async_dispatcher_connect(self.hass, SIGNAL_UPDATE, self.async_write_ha_state)
        )


class CircuitSum(_CircuitEntity):
    _attr_device_class = SensorDeviceClass.POWER
    _attr_state_class = SensorStateClass.MEASUREMENT
    _attr_native_unit_of_measurement = UnitOfPower.WATT

    def __init__(self, rt: PowerRuntime, cid: str, name: str) -> None:
        super().__init__(rt, cid, name, "sum")

    @property
    def native_value(self) -> float | None:
        c = self.rt.engine.circuits.get(self.cid)
        return round(c.total(time.monotonic()), 1) if c else None


class CircuitState(_CircuitEntity):
    _attr_device_class = SensorDeviceClass.ENUM
    _attr_options = [s.lower() for s in STATES]

    def __init__(self, rt: PowerRuntime, cid: str, name: str) -> None:
        super().__init__(rt, cid, name, "state")

    @property
    def native_value(self) -> str | None:
        c = self.rt.engine.circuits.get(self.cid)
        return c.state.lower() if c else None

    @property
    def extra_state_attributes(self) -> dict[str, Any]:
        c = self.rt.engine.circuits.get(self.cid)
        if c is None:
            return {}
        t = time.monotonic()
        return {
            "shed_w": c.cfg.shed_w,
            "restore_w": c.cfg.restore_w,
            "hard_w": c.cfg.hard_w,
            "target_w": c.cfg.target_w,
            "standby_plan": c.standby_plan(t),
            "event_open": c.event_open,
            "rounds": c.rounds,
            "box": self.rt.engine.box(c, t),
        }
