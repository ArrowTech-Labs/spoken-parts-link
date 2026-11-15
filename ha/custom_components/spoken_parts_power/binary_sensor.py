"""Engine heartbeat and printer feed freshness, for Home Assistant side monitoring."""

from __future__ import annotations

import time

from homeassistant.components.binary_sensor import (
    BinarySensorDeviceClass,
    BinarySensorEntity,
)
from homeassistant.core import HomeAssistant
from homeassistant.helpers.dispatcher import async_dispatcher_connect
from homeassistant.helpers.entity_platform import AddConfigEntryEntitiesCallback

from .const import SIGNAL_UPDATE
from .integration import PowerConfigEntry, PowerRuntime


async def async_setup_entry(
    hass: HomeAssistant,
    entry: PowerConfigEntry,
    async_add_entities: AddConfigEntryEntitiesCallback,
) -> None:
    rt = entry.runtime_data
    async_add_entities([EngineOk(rt), FeedOk(rt)])


class _Base(BinarySensorEntity):
    _attr_should_poll = False
    _attr_device_class = BinarySensorDeviceClass.CONNECTIVITY

    def __init__(self, rt: PowerRuntime, key: str, name: str) -> None:
        self.rt = rt
        self._attr_unique_id = f"{rt.entry.entry_id}:{key}"
        self._attr_name = name

    async def async_added_to_hass(self) -> None:
        self.async_on_remove(
            async_dispatcher_connect(self.hass, SIGNAL_UPDATE, self.async_write_ha_state)
        )


class EngineOk(_Base):
    def __init__(self, rt: PowerRuntime) -> None:
        super().__init__(rt, "engine_ok", "SP power engine ok")

    @property
    def is_on(self) -> bool:
        return self.rt.ok()


class FeedOk(_Base):
    def __init__(self, rt: PowerRuntime) -> None:
        super().__init__(rt, "feed_ok", "SP power feed ok")

    @property
    def is_on(self) -> bool:
        eng = self.rt.engine
        return time.monotonic() - eng.feed_at <= eng.tm.feed_stale_s
