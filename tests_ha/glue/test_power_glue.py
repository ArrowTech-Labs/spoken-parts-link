"""Spoken Parts Power glue against a fake ZHA: registry devices and entities, states,
and recorded switch / number / select / update_entity calls. No real plug."""

from __future__ import annotations

import pytest
from homeassistant import config_entries
from homeassistant.core import HomeAssistant, ServiceCall
from homeassistant.data_entry_flow import FlowResultType
from homeassistant.helpers import device_registry as dr
from homeassistant.helpers import entity_registry as er
from pytest_homeassistant_custom_component.common import MockConfigEntry

from custom_components.spoken_parts_power.const import DOMAIN

CID = "11111111-1111-1111-1111-111111111111"
HT = "00:0d:6f:00:00:00:00:01"
PR = "00:0d:6f:00:00:00:00:02"
TUYA = "a4:c1:38:00:00:00:00:09"


class FakeZha:
    def __init__(self, hass: HomeAssistant) -> None:
        self.hass = hass
        self.calls: list[tuple[str, str, dict]] = []
        self.zha = MockConfigEntry(domain="zha", title="ZHA")
        self.zha.add_to_hass(hass)

    def plug(self, ieee: str, base: str, model: str = "3RSP02064Z",
             maker: str = "Third Reality, Inc", watts: float = 0.0) -> None:
        devreg, entreg = dr.async_get(self.hass), er.async_get(self.hass)
        dev = devreg.async_get_or_create(
            config_entry_id=self.zha.entry_id, identifiers={("zha", ieee)},
            manufacturer=maker, model=model, name=base.replace("_", " "))
        for domain, suffix in (("switch", ""), ("sensor", "_power"), ("sensor", "_voltage"),
                               ("number", "_power_rise_threshold"),
                               ("number", "_power_drop_threshold"),
                               ("switch", "_metering_only_mode"),
                               ("select", "_power_on_behavior"), ("update", "_firmware")):
            entreg.async_get_or_create(domain, "zha", f"{ieee}-{domain}{suffix}",
                                       suggested_object_id=f"{base}{suffix}", device_id=dev.id)
        self.hass.states.async_set(f"switch.{base}", "on")
        self.hass.states.async_set(f"sensor.{base}_power", str(watts),
                                   {"unit_of_measurement": "W"})
        self.hass.states.async_set(f"number.{base}_power_rise_threshold", "3200")
        self.hass.states.async_set(f"number.{base}_power_drop_threshold", "3200")
        self.hass.states.async_set(f"switch.{base}_metering_only_mode", "off")
        self.hass.states.async_set(f"select.{base}_power_on_behavior", "On",
                                   {"options": ["Off", "On", "Toggle", "PreviousValue"]})

    def services(self) -> None:
        async def switch(call: ServiceCall) -> None:
            self.calls.append(("switch", call.service, dict(call.data)))
            for eid in _ids(call):
                self.hass.states.async_set(eid, "off" if call.service == "turn_off" else "on")

        async def record(call: ServiceCall) -> None:
            self.calls.append((call.domain, call.service, dict(call.data)))

        for svc in ("turn_on", "turn_off"):
            self.hass.services.async_register("switch", svc, switch)
        self.hass.services.async_register("number", "set_value", record)
        self.hass.services.async_register("select", "select_option", record)
        self.hass.services.async_register("homeassistant", "update_entity", record)


def _ids(call: ServiceCall) -> list[str]:
    ids = call.data.get("entity_id")
    return [ids] if isinstance(ids, str) else list(ids or [])


def config(sustain=0.0):
    return {"version": 3, "site": 1, "circuits": [
        {"id": CID, "name": "Circuit A", "protect": True, "breaker_amps": 15, "volts": 120,
         "shed_w": 1440, "restore_w": 1080, "hard_w": 1800, "unmetered_w": 0,
         "sustain_s": sustain}],
        "plugs": [
            {"ieee": HT, "circuit": CID, "role": "ams_ht", "label": "AMS HT", "rated_w": 300},
            {"ieee": PR, "circuit": CID, "role": "printer", "label": "H2D P1",
             "printer_serial": "01P00A"},
            {"ieee": TUYA, "circuit": CID, "role": "ams_ht", "label": "Not ours"},
        ]}


@pytest.fixture
async def zha(hass: HomeAssistant) -> FakeZha:
    z = FakeZha(hass)
    z.services()
    z.plug(HT, "ams_ht_plug", watts=300)
    z.plug(PR, "plug_for_h2d_p1", watts=600)
    z.plug(TUYA, "fridge_plug", model="TS011F", maker="_TZ3000", watts=100)
    return z


async def setup(hass: HomeAssistant, allow: bool = True) -> MockConfigEntry:
    entry = MockConfigEntry(domain=DOMAIN, title="Spoken Parts Power", unique_id=DOMAIN,
                            options={"allow_switching": allow})
    entry.add_to_hass(hass)
    assert await hass.config_entries.async_setup(entry.entry_id)
    await hass.async_block_till_done()
    return entry


async def ws(client, **msg):
    await client.send_json_auto_id(msg)
    res = await client.receive_json()
    assert res["success"], res
    return res["result"]


async def test_config_flow_single_entry(hass: HomeAssistant, zha) -> None:
    r = await hass.config_entries.flow.async_init(DOMAIN, context={"source": config_entries.SOURCE_USER})
    assert r["type"] is FlowResultType.FORM
    r = await hass.config_entries.flow.async_configure(r["flow_id"], {"allow_switching": True})
    assert r["type"] is FlowResultType.CREATE_ENTRY and r["options"] == {"allow_switching": True}


async def test_inventory_hello_config_and_a_real_shed(hass: HomeAssistant, zha, hass_ws_client) -> None:
    entry = await setup(hass)
    rt = entry.runtime_data
    assert set(rt.inventory) == {HT, PR}, "only THIRDREALITY 3RSP02064Z plugs"
    assert rt.inventory[HT]["ents"]["switch"] == "switch.ams_ht_plug"
    assert rt.inventory[HT]["ents"]["power"] == "sensor.ams_ht_plug_power"
    client = await hass_ws_client(hass)
    hello = await ws(client, type="spoken_parts_power/hello")
    assert hello["id"].startswith("e-") and hello["switching_allowed"] is True
    res = await ws(client, type="spoken_parts_power/config", config=config())
    assert res["version"] == 3
    assert not rt.engine.plugs[TUYA].controllable
    # fresh readings arrive, the circuit arms, then the printer heats up
    hass.states.async_set("sensor.ams_ht_plug_power", "301", {"unit_of_measurement": "W"})
    hass.states.async_set("sensor.plug_for_h2d_p1_power", "601", {"unit_of_measurement": "W"})
    await hass.async_block_till_done()
    rt.on_tick()
    assert rt.engine.circuits[CID].state == "WATCH"
    await ws(client, type="spoken_parts_power/feed",
             printers=[{"serial": "01P00A", "state": "printing", "value_mills": 5000, "age_ms": 100}])
    hass.states.async_set("sensor.plug_for_h2d_p1_power", "1300", {"unit_of_measurement": "W"})
    await hass.async_block_till_done()
    rt.on_tick()
    await hass.async_block_till_done()
    offs = [c for c in zha.calls if c[0] == "switch"]
    assert offs == [("switch", "turn_off", {"entity_id": "switch.ams_ht_plug"})], zha.calls
    assert hass.states.get("switch.plug_for_h2d_p1").state == "on"
    st = await ws(client, type="spoken_parts_power/status")
    circ = st["circuits"][0]
    assert circ["id"] == CID and circ["event"] is not None
    plugs = {p["ieee"]: p for p in st["plugs"]}
    assert plugs[HT]["checks"]["thresholds_1w"] is False
    assert plugs[HT]["checks"]["metering_only_off"] is True
    assert plugs[HT]["checks"]["power_on_previous"] is False
    ent = await ws(client, type="spoken_parts_power/entries", after=0)
    kinds = [e["kind"] for e in ent["entries"]]
    assert "trip" in kinds and "shed" in kinds
    left = await ws(client, type="spoken_parts_power/ack", through=ent["last_seq"])
    assert left["left"] == 0
    assert hass.states.get("sensor.sp_power_circuit_a_state") is not None
    await hass.config_entries.async_unload(entry.entry_id)


async def test_switching_off_locally_switches_nothing(hass: HomeAssistant, zha, hass_ws_client) -> None:
    entry = await setup(hass, allow=False)
    rt = entry.runtime_data
    client = await hass_ws_client(hass)
    await ws(client, type="spoken_parts_power/config", config=config())
    hass.states.async_set("sensor.ams_ht_plug_power", "301", {"unit_of_measurement": "W"})
    hass.states.async_set("sensor.plug_for_h2d_p1_power", "1400", {"unit_of_measurement": "W"})
    await hass.async_block_till_done()
    rt.on_tick()
    rt.on_tick()
    await hass.async_block_till_done()
    assert not [c for c in zha.calls if c[0] == "switch"]
    await hass.config_entries.async_unload(entry.entry_id)


async def test_apply_plug_settings_never_touches_the_relay(hass: HomeAssistant, zha, hass_ws_client) -> None:
    entry = await setup(hass)
    client = await hass_ws_client(hass)
    await ws(client, type="spoken_parts_power/config", config=config())
    await hass.services.async_call(DOMAIN, "apply_plug_settings", {}, blocking=True)
    assert not [c for c in zha.calls if c[0] == "switch"]
    sets = sorted((c[0], c[2]["entity_id"]) for c in zha.calls if c[0] in ("number", "select"))
    assert ("number", "number.ams_ht_plug_power_rise_threshold") in sets
    assert ("number", "number.plug_for_h2d_p1_power_drop_threshold") in sets
    sel = [c for c in zha.calls if c[0] == "select"]
    assert sel and all(c[2]["option"] == "PreviousValue" for c in sel)
    assert not [c for c in zha.calls if "fridge" in str(c[2])]
    await hass.config_entries.async_unload(entry.entry_id)


async def test_person_command_is_refused_for_a_plug_the_engine_did_not_turn_off(
        hass: HomeAssistant, zha, hass_ws_client) -> None:
    entry = await setup(hass)
    client = await hass_ws_client(hass)
    await ws(client, type="spoken_parts_power/config", config=config())
    res = await ws(client, type="spoken_parts_power/command",
                   request={"id": "r1", "kind": "power_on", "plug": PR, "by": "Marc"})
    assert res["result"] == "refused"
    assert not [c for c in zha.calls if c[0] == "switch"]
    await hass.config_entries.async_unload(entry.entry_id)
