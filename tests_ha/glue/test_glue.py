import base64
from unittest.mock import patch

import pytest
import voluptuous as vol
from bleak.backends.device import BLEDevice
from bleak.backends.scanner import AdvertisementData
from homeassistant import config_entries
from homeassistant.components.bluetooth import BluetoothServiceInfoBleak
from homeassistant.core import HomeAssistant
from homeassistant.data_entry_flow import FlowResultType, InvalidData
from homeassistant.exceptions import ServiceValidationError
from homeassistant.helpers.service import async_get_all_descriptions
from pytest_homeassistant_custom_component.common import MockConfigEntry

from custom_components.spoken_parts_printers.const import DOMAIN
from custom_components.spoken_parts_printers.drivers import registry
from custom_components.spoken_parts_printers.transports.fake import FakeTransport

ADDR = "AA:BB:CC:DD:EE:FF"
UID = "nelko.p21:aa:bb:cc:dd:ee:ff"
BUILD = "custom_components.spoken_parts_printers.integration.build_transport"


def p21_reply(data):
    if data.startswith(b"\x1b!?"):
        return b"\x00"
    if data.startswith(b"BATTERY?"):
        return b"BATTERY \x77\x00\r\n"
    return None


@pytest.fixture
def fake():
    t = FakeTransport()
    t.responder = p21_reply
    with patch(BUILD, return_value=t):
        yield t


async def add_entry(hass: HomeAssistant, **data) -> MockConfigEntry:
    entry = MockConfigEntry(
        domain=DOMAIN,
        unique_id=UID,
        title="Label maker",
        data={"driver": "nelko.p21", "address": ADDR, "name": "Label maker",
              "transport": "spp", "media": "14x40", **data},
    )
    entry.add_to_hass(hass)
    assert await hass.config_entries.async_setup(entry.entry_id)
    await hass.async_block_till_done()
    return entry


def job(**over):
    data = bytes(12 * 320)
    call = {
        "printer_id": UID, "job_id": "j1", "driver": "nelko.p21", "document_type": "spool_label",
        "media": {"id": "14x40", "kind": "die_cut", "width_mm": 14, "length_mm": 40},
        "copies": 1, "pages": [{"width": 96, "height": 320, "data_b64": base64.b64encode(data).decode()}],
    }
    call.update(over)
    return call


async def test_config_flow_creates_entry(hass: HomeAssistant, fake) -> None:
    r = await hass.config_entries.flow.async_init(DOMAIN, context={"source": config_entries.SOURCE_USER})
    assert r["type"] is FlowResultType.FORM and r["step_id"] == "user"
    r = await hass.config_entries.flow.async_configure(r["flow_id"], {"driver": "nelko.p21"})
    assert r["step_id"] == "printer"
    bad = await hass.config_entries.flow.async_configure(
        r["flow_id"], {"address": "nope", "name": "x", "transport": "spp", "media": "14x40"}
    )
    assert bad["errors"] == {"address": "invalid_address"}
    bad = await hass.config_entries.flow.async_configure(
        r["flow_id"], {"address": ADDR, "name": "x", "transport": "spp", "media": "99x99"}
    )
    assert bad["errors"] == {"media": "bad_media"}
    r = await hass.config_entries.flow.async_configure(
        r["flow_id"],
        {"address": "aa-bb-cc-dd-ee-ff", "name": "Label maker", "transport": "spp", "media": "14x40"},
    )
    await hass.async_block_till_done()
    assert r["type"] is FlowResultType.CREATE_ENTRY
    assert r["title"] == "Label maker"
    assert r["data"] == {"driver": "nelko.p21", "address": ADDR, "name": "Label maker",
                         "transport": "spp", "media": "14x40"}
    assert r["result"].unique_id == UID

    # Same printer again: aborts.
    r = await hass.config_entries.flow.async_init(DOMAIN, context={"source": config_entries.SOURCE_USER})
    r = await hass.config_entries.flow.async_configure(r["flow_id"], {"driver": "nelko.p21"})
    r = await hass.config_entries.flow.async_configure(
        r["flow_id"], {"address": ADDR, "name": "y", "transport": "ble", "media": "14x50"}
    )
    assert r["type"] is FlowResultType.ABORT and r["reason"] == "already_configured"


async def test_second_driver_in_config_flow_choices(hass: HomeAssistant) -> None:
    from tests_ha_helpers_fake4x6 import Fake4x6  # noqa: PLC0415

    registry.register(Fake4x6())
    try:
        r = await hass.config_entries.flow.async_init(DOMAIN, context={"source": config_entries.SOURCE_USER})
        selector = r["data_schema"].schema["driver"]
        values = [o["value"] for o in selector.config["options"]]
        assert values == ["acme.ship4x6", "nelko.p21", "nelko.pm220"]
        r = await hass.config_entries.flow.async_configure(r["flow_id"], {"driver": "acme.ship4x6"})
        media = r["data_schema"].schema["media"]
        assert [o["value"] for o in media.config["options"]] == ["102x152"]
    finally:
        registry.unregister("acme.ship4x6")


async def test_pm220_offers_spp_only(hass: HomeAssistant, fake) -> None:
    r = await hass.config_entries.flow.async_init(DOMAIN, context={"source": config_entries.SOURCE_USER})
    r = await hass.config_entries.flow.async_configure(r["flow_id"], {"driver": "nelko.pm220"})
    transport = r["data_schema"].schema["transport"]
    assert [o["value"] for o in transport.config["options"]] == ["spp"]
    with pytest.raises(InvalidData):
        await hass.config_entries.flow.async_configure(
            r["flow_id"], {"address": ADDR, "name": "x", "transport": "ble", "media": "50x30"}
        )
    # A custom label size inside the PM220 range is accepted.
    r = await hass.config_entries.flow.async_configure(
        r["flow_id"], {"address": ADDR, "name": "Shelf", "transport": "spp", "media": "45x25"}
    )
    assert r["type"] is FlowResultType.CREATE_ENTRY and r["data"]["media"] == "45x25"


def _service_info(name: str) -> BluetoothServiceInfoBleak:
    return BluetoothServiceInfoBleak(
        name=name, address=ADDR, rssi=-60, manufacturer_data={}, service_data={},
        service_uuids=[], source="local", device=BLEDevice(ADDR, name, None),
        advertisement=AdvertisementData(local_name=name, manufacturer_data={}, service_data={},
                                        service_uuids=[], tx_power=None, rssi=-60, platform_data=()),
        connectable=True, time=0, tx_power=None,
    )


async def test_bluetooth_discovery(hass: HomeAssistant, fake) -> None:
    r = await hass.config_entries.flow.async_init(
        DOMAIN, context={"source": config_entries.SOURCE_BLUETOOTH}, data=_service_info("P21")
    )
    assert r["type"] is FlowResultType.FORM and r["step_id"] == "printer"
    assert r["data_schema"]({"name": "n", "transport": "spp", "media": "14x40"})["address"] == ADDR
    r = await hass.config_entries.flow.async_configure(
        r["flow_id"], {"address": ADDR, "name": "Pocket", "transport": "spp", "media": "14x40"}
    )
    await hass.async_block_till_done()
    assert r["type"] is FlowResultType.CREATE_ENTRY and r["result"].unique_id == UID

    r = await hass.config_entries.flow.async_init(
        DOMAIN, context={"source": config_entries.SOURCE_BLUETOOTH}, data=_service_info("Speaker")
    )
    assert r["type"] is FlowResultType.ABORT and r["reason"] == "not_supported"


async def test_services_and_list_printers(hass: HomeAssistant, fake) -> None:
    await add_entry(hass)
    assert hass.services.has_service(DOMAIN, "list_printers")
    assert hass.services.has_service(DOMAIN, "print_raster")
    resp = await hass.services.async_call(DOMAIN, "list_printers", {}, blocking=True, return_response=True)
    assert resp["version"] == "0.1.0"
    assert len(resp["printers"]) == 1
    p = resp["printers"][0]
    assert p == {
        "id": UID, "name": "Label maker", "driver": "nelko.p21", "transport": "fake",
        "address": ADDR, "state": "ready", "last_error": "", "battery": 77, "media": "14x40",
        "last_seen": p["last_seen"],
    }
    assert p["last_seen"].endswith("Z")

    # services.yaml loads and describes both services.
    descs = await async_get_all_descriptions(hass)
    assert set(descs[DOMAIN]) == {"list_printers", "print_raster"}
    assert "pages" in descs[DOMAIN]["print_raster"]["fields"]


async def test_print_raster(hass: HomeAssistant, fake) -> None:
    await add_entry(hass)
    resp = await hass.services.async_call(DOMAIN, "print_raster", job(), blocking=True, return_response=True)
    assert resp["ok"] is True and resp["pages"] == 1 and resp["bytes"] > 3840
    assert b"BITMAP 0,0,12,320,1," in fake.written
    state = hass.states.get("sensor.label_maker_status")
    assert state.state == "ready"
    assert state.attributes["driver"] == "nelko.p21"
    assert state.attributes["last_print"]["ok"] is True
    assert hass.states.get("sensor.label_maker_battery").state == "77"

    # Printer problems answer, they do not raise.
    fake.fail_connect = True
    await fake.close()
    resp = await hass.services.async_call(DOMAIN, "print_raster", job(), blocking=True, return_response=True)
    assert resp["ok"] is False and resp["code"] == "offline"
    assert hass.states.get("sensor.label_maker_status").state == "offline"

    resp = await hass.services.async_call(
        DOMAIN, "print_raster", job(printer_id="nelko.p21:00:00:00:00:00:00"), blocking=True, return_response=True
    )
    assert resp["code"] == "unknown_printer"

    resp = await hass.services.async_call(
        DOMAIN, "print_raster", job(driver="nelko.pm220"), blocking=True, return_response=True
    )
    assert resp["code"] == "bad_format"

    # The link omits media and copies when a job has none.
    fake.fail_connect = False
    call = job()
    del call["media"], call["copies"]
    resp = await hass.services.async_call(DOMAIN, "print_raster", call, blocking=True, return_response=True)
    assert resp["ok"] is True


async def test_print_raster_malformed_call_raises(hass: HomeAssistant, fake) -> None:
    await add_entry(hass)
    call = job()
    del call["pages"]
    with pytest.raises((vol.Invalid, ServiceValidationError)):
        await hass.services.async_call(DOMAIN, "print_raster", call, blocking=True, return_response=True)


async def test_options_flow_changes_media(hass: HomeAssistant, fake) -> None:
    entry = await add_entry(hass)
    r = await hass.config_entries.options.async_init(entry.entry_id)
    assert r["step_id"] == "init"
    r = await hass.config_entries.options.async_configure(
        r["flow_id"],
        {"name": "Shelf printer", "transport": "spp", "media": "14x50", "keep_connected": True, "rfcomm_channel": 1},
    )
    assert r["type"] is FlowResultType.CREATE_ENTRY
    await hass.async_block_till_done()
    assert entry.title == "Shelf printer"
    assert entry.runtime_data.media_id == "14x50"
    assert entry.runtime_data.name == "Shelf printer"


async def test_unload(hass: HomeAssistant, fake) -> None:
    entry = await add_entry(hass)
    assert await hass.config_entries.async_unload(entry.entry_id)
    resp = await hass.services.async_call(DOMAIN, "list_printers", {}, blocking=True, return_response=True)
    assert resp["printers"] == []
