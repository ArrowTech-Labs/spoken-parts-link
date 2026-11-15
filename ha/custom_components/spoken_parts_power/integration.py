"""Home Assistant glue for Spoken Parts Power.

The engine (engine/core.py) runs inside Home Assistant's event loop:

- Plug reports arrive as state events of each plug's power sensor and switch
  (state changed, and state reported for unchanged readings).
- The engine is ticked every 250 ms and on every report.
- It switches plugs with switch.turn_off / switch.turn_on, only plugs that are
  THIRDREALITY Smart Plug Gen3 (3RSP02064Z) devices on ZHA, only the relay
  entity of that device, and only when the local option "Allow switching" is on.
- It reads a plug directly with homeassistant.update_entity on its power sensor.

The Spoken Parts Link app talks to it over Home Assistant's own websocket (through
the Supervisor), with the spoken_parts_power/* commands below. Nothing here talks
to the internet or to a 3D printer.
"""

from __future__ import annotations

import asyncio
import logging
import time
import uuid
from datetime import timedelta
from typing import Any

import voluptuous as vol

from homeassistant.components import websocket_api
from homeassistant.config_entries import ConfigEntry
from homeassistant.const import (
    EVENT_STATE_CHANGED,
    EVENT_STATE_REPORTED,
    Platform,
)
from homeassistant.const import __version__ as HA_VERSION
from homeassistant.core import Event, HomeAssistant, ServiceCall, callback
from homeassistant.helpers import config_validation as cv
from homeassistant.helpers import device_registry as dr
from homeassistant.helpers import entity_registry as er
from homeassistant.helpers.dispatcher import async_dispatcher_send
from homeassistant.helpers.event import async_track_time_interval
from homeassistant.helpers.service import async_register_admin_service
from homeassistant.helpers.storage import Store
from homeassistant.helpers.typing import ConfigType
from homeassistant.util import dt as dt_util

from .const import (
    CMD_TIMEOUT_S,
    CONF_ALLOW_SWITCHING,
    DOMAIN,
    INVENTORY_EVERY_S,
    SAVE_DELAY_S,
    SERVICE_APPLY_PLUG_SETTINGS,
    SIGNAL_UPDATE,
    STORE_KEY,
    STORE_VERSION,
    TICK_S,
    VERSION,
)
from .engine import Engine
from .inventory import MODEL, checks, classify, ieee_of, is_supported, previous_option

_LOGGER = logging.getLogger(__name__)

PLATFORMS = [Platform.SENSOR, Platform.BINARY_SENSOR]
CONFIG_SCHEMA = cv.config_entry_only_config_schema(DOMAIN)

type PowerConfigEntry = ConfigEntry[PowerRuntime]


def _wall() -> str:
    return dt_util.utcnow().isoformat(timespec="milliseconds").replace("+00:00", "Z")


class PowerRuntime:
    """The engine, its actuator, the plug inventory and storage."""

    def __init__(self, hass: HomeAssistant, entry: ConfigEntry) -> None:
        self.hass = hass
        self.entry = entry
        self.allow = bool(entry.options.get(CONF_ALLOW_SWITCHING, True))
        self.engine = Engine(self, clock=time.monotonic, wall=_wall)
        self.engine.switching_allowed = self.allow
        self.store: Store[dict[str, Any]] = Store(hass, STORE_VERSION, STORE_KEY)
        self.inventory: dict[str, dict[str, Any]] = {}
        self.by_entity: dict[str, tuple[str, str]] = {}
        self.boot_id = uuid.uuid4().hex[:12]
        self.engine_id = ""
        self.last_tick = 0.0
        self.last_push = 0.0
        self.last_seq_saved = -1
        self.save_due = False
        self.settings_result: dict[str, Any] | None = None
        self.tasks: set[asyncio.Task] = set()

    # ---- the actuator the engine calls ---------------------------------------------------
    def switch(self, key: str, action: str, t: float) -> None:
        inv = self.inventory.get(key)
        eid = inv["ents"].get("switch") if inv else None
        if not inv or not inv["model_ok"] or not eid or not self.allow:
            # Refused here as well as in the engine: reported back as a failed command.
            self.hass.loop.call_soon(self._command_done, key, action, False)
            return
        self._task(self._switch(key, eid, action))

    def read(self, key: str, t: float) -> bool:
        inv = self.inventory.get(key)
        eid = inv["ents"].get("power") if inv else None
        if not eid:
            return False
        st = self.hass.states.get(eid)
        if st is None or st.state == "unavailable":
            return False
        self._task(self._read(eid))
        return True

    def _task(self, coro: Any) -> None:
        task = self.hass.async_create_background_task(coro, f"{DOMAIN} plug call")
        self.tasks.add(task)
        task.add_done_callback(self.tasks.discard)

    async def _switch(self, key: str, eid: str, action: str) -> None:
        ok = False
        try:
            async with asyncio.timeout(CMD_TIMEOUT_S):
                await self.hass.services.async_call(
                    "switch", f"turn_{action}", {"entity_id": eid}, blocking=True
                )
            ok = True
        except Exception as err:  # noqa: BLE001 (a failed command is an engine input, not a crash)
            _LOGGER.warning("Turning %s %s failed: %s", eid, action, type(err).__name__)
        self._command_done(key, action, ok)

    @callback
    def _command_done(self, key: str, action: str, ok: bool) -> None:
        t = time.monotonic()
        self.engine.on_command(key, action, ok, t)
        self.engine.tick(t)

    async def _read(self, eid: str) -> None:
        try:
            async with asyncio.timeout(5):
                await self.hass.services.async_call(
                    "homeassistant", "update_entity", {"entity_id": eid}, blocking=True
                )
        except Exception as err:  # noqa: BLE001
            _LOGGER.debug("Reading %s failed: %s", eid, type(err).__name__)

    # ---- plugs ---------------------------------------------------------------------
    @callback
    def refresh_inventory(self, *_: Any) -> None:
        devreg = dr.async_get(self.hass)
        entreg = er.async_get(self.hass)
        inv: dict[str, dict[str, Any]] = {}
        for dev in devreg.devices.values():
            model = (getattr(dev, "model_id", None) or dev.model or "").strip()
            if model.upper() != MODEL and (dev.model or "").strip().upper() != MODEL:
                continue
            ieee = ieee_of(dev.identifiers)
            if not ieee:
                continue
            ents = [{
                "entity_id": e.entity_id,
                "platform": e.platform,
                "unique_id": e.unique_id,
                "translation_key": e.translation_key,
                "original_name": e.original_name,
                "entity_category": str(e.entity_category) if e.entity_category else None,
                "device_class": e.device_class or e.original_device_class,
            } for e in er.async_entries_for_device(entreg, dev.id) if not e.disabled_by]
            inv[ieee] = {
                "ieee": ieee,
                "ha_name": dev.name_by_user or dev.name or ieee,
                "model": MODEL,
                "manufacturer": dev.manufacturer or "",
                "device_id": dev.id,
                "ents": classify(ents),
                "model_ok": is_supported(MODEL, dev.manufacturer),
            }
        self.inventory = inv
        by_entity: dict[str, tuple[str, str]] = {}
        for ieee, d in inv.items():
            for role in ("switch", "power"):
                if d["ents"].get(role):
                    by_entity[d["ents"][role]] = (ieee, role)
        self.by_entity = by_entity
        self._mark_controllable()

    @callback
    def periodic(self, _now: Any = None) -> None:
        self.refresh_inventory()

    def _mark_controllable(self) -> None:
        for key, p in self.engine.plugs.items():
            inv = self.inventory.get(key)
            p.controllable = bool(inv and inv["model_ok"] and inv["ents"].get("switch")
                                  and inv["ents"].get("power"))
            if not inv and p.available:
                # Not a plug Home Assistant has: it cannot be measured or switched.
                self.engine.on_reachable(key, False, time.monotonic())

    @callback
    def seed_states(self) -> None:
        """Current states, with their real age, so nothing counts as fresh."""
        for eid, (ieee, role) in self.by_entity.items():
            st = self.hass.states.get(eid)
            if st is None:
                continue
            t = self._measured(st)
            self._apply_state(ieee, role, st, t)

    @staticmethod
    def _measured(st: Any) -> float:
        """When Home Assistant got this reading, on the monotonic clock."""
        when = getattr(st, "last_reported", None) or st.last_updated
        return time.monotonic() - max(0.0, (dt_util.utcnow() - when).total_seconds())

    def _apply_state(self, ieee: str, role: str, st: Any, t: float) -> None:
        if role == "power":
            if st.state == "unavailable":
                self.engine.on_reachable(ieee, False, t)
                return
            try:
                w = float(st.state)
            except (TypeError, ValueError):
                return
            if st.attributes.get("unit_of_measurement") == "kW":
                w *= 1000.0
            # Arrival now, measured when Home Assistant got it. A valid reading also
            # marks the plug reachable again.
            self.engine.on_power(ieee, w, time.monotonic(), measured=self._measured(st))
        else:
            self.engine.on_relay(ieee, st.state, t)

    @callback
    def _event_filter(self, data: Any) -> bool:
        return data.get("entity_id") in self.by_entity

    @callback
    def on_state_event(self, event: Event) -> None:
        hit = self.by_entity.get(event.data.get("entity_id"))
        new = event.data.get("new_state")
        if hit is None or new is None:
            return
        t = time.monotonic()
        self._apply_state(hit[0], hit[1], new, t)
        self.engine.tick(t)
        self._schedule_save()

    # ---- clock and storage -------------------------------------------------------------
    @callback
    def on_tick(self, _now: Any = None) -> None:
        t = time.monotonic()
        self.engine.tick(t)
        self.last_tick = t
        if t - self.last_push >= 1.0:
            self.last_push = t
            async_dispatcher_send(self.hass, SIGNAL_UPDATE)
        self._schedule_save()

    @callback
    def _schedule_save(self) -> None:
        """Save within SAVE_DELAY_S of the first unsaved entry. Not re-armed while one is
        due, so a steady stream of entries cannot keep pushing the save back."""
        if self.engine.journal.seq != self.last_seq_saved and not self.save_due:
            self.save_due = True
            self.store.async_delay_save(self._store_data, SAVE_DELAY_S)

    def _store_data(self) -> dict[str, Any]:
        self.save_due = False
        self.last_seq_saved = self.engine.journal.seq
        data = self.engine.to_store(time.monotonic())
        data["engine_id"] = self.engine_id
        return data

    async def load(self) -> None:
        data = await self.store.async_load() or {}
        self.engine_id = data.get("engine_id") or f"e-{uuid.uuid4().hex[:16]}"
        self.engine.engine_id = self.engine_id
        self.refresh_inventory()
        self.engine.from_store(data, time.monotonic())
        self._mark_controllable()
        self.seed_states()

    # ---- what the link sees ---------------------------------------------------------------
    def ok(self) -> bool:
        return time.monotonic() - self.last_tick < 5 and not any(
            c.state == "OFF" and c.cfg.protect and c.plugs for c in self.engine.circuits.values()
        )

    def hello(self) -> dict[str, Any]:
        return {
            "id": self.engine_id,
            "boot_id": self.boot_id,
            "version": VERSION,
            "ha_version": HA_VERSION,
            "switching_allowed": self.allow,
            "config_version": self.engine.config_version,
            "feed_ok": time.monotonic() - self.engine.feed_at <= self.engine.tm.feed_stale_s,
            "ok": self.ok(),
            "faults": self.engine.faults,
            # Circuits the engine refused from the last config (short reasons).
            "errors": [str(e)[:200] for e in self.engine.config_errors][:20],
        }

    def status(self) -> dict[str, Any]:
        t = time.monotonic()
        st = self.engine.status(t)
        plugs = []
        for ieee, inv in sorted(self.inventory.items()):
            ps = self.engine.plug_status(ieee, t)
            states = {}
            for eid in inv["ents"].values():
                if eid:
                    s = self.hass.states.get(eid)
                    states[eid] = s.state if s else None
            ck = checks(inv["ents"], states, inv["model_ok"])
            ck["heartbeat_ok"] = ps["heartbeat_ok"]
            ck["reports_fast"] = ps["reports_fast"]
            plugs.append({
                "ieee": ieee,
                "ha_name": inv["ha_name"],
                "model": inv["model"],
                "switch_entity": inv["ents"].get("switch") or "",
                "power_entity": inv["ents"].get("power") or "",
                "w": ps["w"], "age_s": ps["age_s"], "relay": ps["relay"],
                "reachable": ps["reachable"], "held_off": ps["held_off"],
                "learned_w": ps["learned_w"], "peak_w": ps["peak_w"],
                "checks": ck,
            })
        return {"engine": self.hello(), "circuits": st["circuits"], "plugs": plugs,
                "errors": st["errors"], "settings": self.settings_result}

    @callback
    def apply_config(self, doc: dict[str, Any]) -> dict[str, Any]:
        res = self.engine.apply_config(doc, time.monotonic())
        self._mark_controllable()
        self.save_due = True
        self.store.async_delay_save(self._store_data, 1)
        async_dispatcher_send(self.hass, SIGNAL_UPDATE)
        return res

    async def apply_plug_settings(self) -> dict[str, Any]:
        """Thresholds to 1 W and power-on behavior to "previous" on the plugs that are on
        a circuit. Never touches the relay. Only when an admin calls the service."""
        out = []
        for key, p in sorted(self.engine.plugs.items()):
            inv = self.inventory.get(key)
            if p.circuit is None or not inv or not inv["model_ok"]:
                continue
            ents, done, failed = inv["ents"], [], []
            calls: list[tuple[str, str, dict[str, Any]]] = []
            for role in ("rise", "drop"):
                if ents.get(role):
                    calls.append(("number", "set_value", {"entity_id": ents[role], "value": 1}))
            if ents.get("power_on"):
                st = self.hass.states.get(ents["power_on"])
                opt = previous_option(st.attributes.get("options") if st else None)
                if opt:
                    calls.append(("select", "select_option",
                                  {"entity_id": ents["power_on"], "option": opt}))
            for domain, service, data in calls:
                assert domain in ("number", "select")  # never the relay
                try:
                    async with asyncio.timeout(15):
                        await self.hass.services.async_call(domain, service, data, blocking=True)
                    done.append(data["entity_id"])
                except Exception as err:  # noqa: BLE001
                    failed.append(f"{data['entity_id']}: {type(err).__name__}")
            out.append({"plug": key, "name": inv["ha_name"], "set": done, "failed": failed})
        self.settings_result = {"at": _wall(), "plugs": out}
        _LOGGER.info("Spoken Parts Power plug settings: %s", self.settings_result)
        return self.settings_result

    async def close(self) -> None:
        for task in list(self.tasks):
            task.cancel()
        await self.store.async_save(self._store_data())


def _runtime(hass: HomeAssistant) -> PowerRuntime | None:
    return hass.data.get(DOMAIN)


# ---- websocket commands (the link's only way in) -------------------------------------------


def _with_runtime(fn: Any) -> Any:
    @callback
    def handler(hass: HomeAssistant, connection: websocket_api.ActiveConnection,
                msg: dict[str, Any]) -> None:
        rt = _runtime(hass)
        if rt is None:
            connection.send_error(msg["id"], "not_ready", "Spoken Parts Power is not set up")
            return
        try:
            connection.send_result(msg["id"], fn(rt, msg))
        except Exception as err:  # noqa: BLE001
            _LOGGER.exception("spoken_parts_power command failed")
            connection.send_error(msg["id"], "failed", type(err).__name__)

    return handler


@websocket_api.websocket_command({vol.Required("type"): "spoken_parts_power/hello"})
@websocket_api.require_admin
@callback
def ws_hello(hass: HomeAssistant, connection: Any, msg: dict[str, Any]) -> None:
    _with_runtime(lambda rt, m: rt.hello())(hass, connection, msg)


@websocket_api.websocket_command({vol.Required("type"): "spoken_parts_power/status"})
@websocket_api.require_admin
@callback
def ws_status(hass: HomeAssistant, connection: Any, msg: dict[str, Any]) -> None:
    _with_runtime(lambda rt, m: rt.status())(hass, connection, msg)


def _feed(rt: PowerRuntime, msg: dict[str, Any]) -> dict[str, Any]:
    t = time.monotonic()
    rt.engine.on_feed({"printers": msg.get("printers") or []}, t)
    rt.engine.tick(t)
    return {"ok": True}


@websocket_api.websocket_command({
    vol.Required("type"): "spoken_parts_power/feed",
    vol.Required("printers"): vol.All([dict], vol.Length(max=200)),
})
@websocket_api.require_admin
@callback
def ws_feed(hass: HomeAssistant, connection: Any, msg: dict[str, Any]) -> None:
    _with_runtime(_feed)(hass, connection, msg)


@websocket_api.websocket_command({
    vol.Required("type"): "spoken_parts_power/config",
    vol.Required("config"): dict,
})
@websocket_api.require_admin
@callback
def ws_config(hass: HomeAssistant, connection: Any, msg: dict[str, Any]) -> None:
    _with_runtime(lambda rt, m: rt.apply_config(m["config"]))(hass, connection, msg)


def _command(rt: PowerRuntime, msg: dict[str, Any]) -> dict[str, Any]:
    t = time.monotonic()
    out = rt.engine.command(msg["request"], t)
    rt.engine.tick(t)
    return out


@websocket_api.websocket_command({
    vol.Required("type"): "spoken_parts_power/command",
    vol.Required("request"): {
        vol.Required("id"): str,
        vol.Required("kind"): vol.In(["power_on", "retry"]),
        vol.Required("plug"): str,
        vol.Optional("by"): str,
    },
})
@websocket_api.require_admin
@callback
def ws_command(hass: HomeAssistant, connection: Any, msg: dict[str, Any]) -> None:
    _with_runtime(_command)(hass, connection, msg)


@websocket_api.websocket_command({
    vol.Required("type"): "spoken_parts_power/entries",
    vol.Optional("after", default=0): vol.All(int, vol.Range(min=0)),
    vol.Optional("limit", default=200): vol.All(int, vol.Range(min=1, max=500)),
})
@websocket_api.require_admin
@callback
def ws_entries(hass: HomeAssistant, connection: Any, msg: dict[str, Any]) -> None:
    _with_runtime(lambda rt, m: {
        "engine_id": rt.engine_id,
        "entries": rt.engine.journal.after(m["after"], m["limit"]),
        "last_seq": rt.engine.journal.seq,
        "dropped": rt.engine.journal.dropped,
    })(hass, connection, msg)


@websocket_api.websocket_command({
    vol.Required("type"): "spoken_parts_power/ack",
    vol.Required("through"): vol.All(int, vol.Range(min=0)),
})
@websocket_api.require_admin
@callback
def ws_ack(hass: HomeAssistant, connection: Any, msg: dict[str, Any]) -> None:
    _with_runtime(lambda rt, m: {"left": rt.engine.journal.ack(m["through"])})(
        hass, connection, msg)


WS_COMMANDS = (ws_hello, ws_status, ws_feed, ws_config, ws_command, ws_entries, ws_ack)


async def async_setup(hass: HomeAssistant, config: ConfigType) -> bool:
    for cmd in WS_COMMANDS:
        websocket_api.async_register_command(hass, cmd)

    async def apply_settings(call: ServiceCall) -> None:
        rt = _runtime(hass)
        if rt is not None:
            await rt.apply_plug_settings()

    async_register_admin_service(
        hass, DOMAIN, SERVICE_APPLY_PLUG_SETTINGS, apply_settings, vol.Schema({})
    )
    return True


async def async_setup_entry(hass: HomeAssistant, entry: PowerConfigEntry) -> bool:
    rt = PowerRuntime(hass, entry)
    await rt.load()
    entry.runtime_data = rt
    hass.data[DOMAIN] = rt
    for event_type in (EVENT_STATE_CHANGED, EVENT_STATE_REPORTED):
        entry.async_on_unload(
            hass.bus.async_listen(event_type, rt.on_state_event, event_filter=rt._event_filter)
        )
    entry.async_on_unload(
        async_track_time_interval(hass, rt.on_tick, timedelta(seconds=TICK_S),
                                  name=f"{DOMAIN} tick")
    )
    entry.async_on_unload(
        async_track_time_interval(
            hass, rt.periodic,
            timedelta(seconds=INVENTORY_EVERY_S), name=f"{DOMAIN} inventory")
    )
    entry.async_on_unload(
        hass.bus.async_listen(dr.EVENT_DEVICE_REGISTRY_UPDATED, rt.refresh_inventory)
    )
    entry.async_on_unload(entry.add_update_listener(_async_options_updated))
    await hass.config_entries.async_forward_entry_setups(entry, PLATFORMS)
    return True


async def _async_options_updated(hass: HomeAssistant, entry: PowerConfigEntry) -> None:
    rt = entry.runtime_data
    rt.allow = bool(entry.options.get(CONF_ALLOW_SWITCHING, True))
    rt.engine.switching_allowed = rt.allow
    async_dispatcher_send(hass, SIGNAL_UPDATE)


async def async_unload_entry(hass: HomeAssistant, entry: PowerConfigEntry) -> bool:
    ok = await hass.config_entries.async_unload_platforms(entry, PLATFORMS)
    if ok:
        await entry.runtime_data.close()
        hass.data.pop(DOMAIN, None)
    return ok
