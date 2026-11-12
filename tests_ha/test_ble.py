import asyncio
import math

import pytest
from conftest import run

from custom_components.spoken_parts_printers.transports.base import TransportError
from custom_components.spoken_parts_printers.transports.ble import BleTransport


class FakeBleClient:
    def __init__(self, fail_on_write=None):
        self.is_connected = True
        self.writes = []
        self.notify = {}
        self.fail_on_write = fail_on_write
        self.disconnected = False

    async def start_notify(self, uuid, cb):
        self.notify[uuid] = cb

    async def write_gatt_char(self, uuid, data, response=False):
        if self.fail_on_write is not None and len(self.writes) == self.fail_on_write:
            raise OSError("gatt error")
        self.writes.append((uuid, bytes(data), response))

    async def disconnect(self):
        self.disconnected = True
        self.is_connected = False


def make(p21, client=None, device="dev"):
    sleeps = []
    client = client or FakeBleClient()

    async def sleep(s):
        sleeps.append(s)

    async def connector(dev, on_disconnect):
        assert dev == "dev"
        return client

    t = BleTransport(
        "aa:bb:cc:dd:ee:ff", p21.ble, device_getter=lambda: device, connector=connector, sleep=sleep
    )
    return t, client, sleeps


def test_chunking_and_pacing(p21):
    t, client, sleeps = make(p21)
    data = bytes(range(256)) * 16  # 4096 bytes

    async def go():
        await t.connect()
        return await t.write(data)

    assert run(go()) == len(data)
    n = math.ceil(len(data) / 20)
    assert len(client.writes) == n
    assert all(len(c) <= 20 for _, c, _ in client.writes)
    assert b"".join(c for _, c, _ in client.writes) == data
    assert {u for u, _, _ in client.writes} == {p21.ble.write_uuid}
    assert {r for _, _, r in client.writes} == {False}  # write without response
    assert sleeps == [0.012] * (n - 1)
    assert p21.ble.notify_uuid in client.notify


def test_notify_is_read(p21):
    t, client, _ = make(p21)

    async def go():
        await t.connect()

        async def printer_answers():
            await asyncio.sleep(0.01)
            client.notify[p21.ble.notify_uuid](None, bytearray(b"\x00"))

        asyncio.get_running_loop().create_task(printer_answers())
        return await t.request(p21.status_query(), p21.parse_status, timeout=1.0)

    st = run(go())
    assert st.ok and st.raw == 0
    assert client.writes[0][1] == b"\x1b!?\r\n"


def test_not_seen_over_le_is_offline(p21):
    t, _, _ = make(p21, device=None)
    with pytest.raises(TransportError) as e:
        run(t.connect())
    assert e.value.code == "offline"
    assert "spp" in e.value.message


def test_write_error_disconnects(p21):
    t, client, _ = make(p21, FakeBleClient(fail_on_write=3))

    async def go():
        await t.connect()
        await t.write(bytes(200))

    with pytest.raises(TransportError) as e:
        run(go())
    assert e.value.code == "write_failed"
    assert "60 of 200" in e.value.message
    assert client.disconnected and not t.is_connected


def test_connector_error_is_offline(p21):
    async def connector(dev, cb):
        raise RuntimeError("no slot")

    t = BleTransport("aa:bb:cc:dd:ee:ff", p21.ble, device_getter=lambda: "dev", connector=connector)
    with pytest.raises(TransportError) as e:
        run(t.connect())
    assert e.value.code == "offline"


def test_ble_write_seconds_counts_the_pacing():
    from custom_components.spoken_parts_printers.drivers.base import BleParams
    from custom_components.spoken_parts_printers.transports.ble import BleTransport

    params = BleParams(service_uuid="ff00", write_uuid="ff02", notify_uuid="ff01", chunk_size=20, pacing_s=0.012)
    t = BleTransport("aa:bb:cc:dd:ee:ff", params, device_getter=lambda: None, connector=None)
    assert abs(t.write_seconds(7200) - 4.32) < 0.01
