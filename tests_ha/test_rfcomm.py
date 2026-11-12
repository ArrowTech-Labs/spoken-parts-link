import errno
import socket

import pytest
from conftest import run

from custom_components.spoken_parts_printers.transports import rfcomm
from custom_components.spoken_parts_printers.transports.base import TransportError
from custom_components.spoken_parts_printers.transports.rfcomm import RfcommTransport


class FakeSocket:
    def __init__(self, connect_error=None, send_error=None, replies=(), answers=None):
        self.answers = answers or {}
        self.connect_error = connect_error
        self.send_error = send_error
        self.replies = list(replies)
        self.sent = []
        self.address = None
        self.closed = False
        self.timeouts = []

    def settimeout(self, t):
        self.timeouts.append(t)

    def connect(self, addr):
        if self.connect_error:
            raise self.connect_error
        self.address = addr

    def sendall(self, data):
        if self.send_error and len(self.sent) >= 1:
            raise self.send_error
        self.sent.append(bytes(data))
        if bytes(data) in self.answers:
            self.replies.append(self.answers[bytes(data)])

    def recv(self, n):
        if not self.replies:
            raise TimeoutError()
        r = self.replies.pop(0)
        return r

    def shutdown(self, how):
        pass

    def close(self):
        self.closed = True


def transport(p21, sock, **kw):
    return RfcommTransport("aa:bb:cc:dd:ee:ff", p21.spp, socket_factory=lambda: sock, **kw)


def test_connect_write_read(p21):
    # A stale byte waiting from earlier is drained; the answer comes after the query.
    sock = FakeSocket(replies=[b"\x04"], answers={b"\x1b!?\r\n": b"\x00"})
    t = transport(p21, sock)

    async def go():
        await t.connect()
        n = await t.write(bytes(1300))
        st = await t.request(p21.status_query(), p21.parse_status, timeout=1)
        return n, st

    n, st = run(go())
    assert sock.address == ("AA:BB:CC:DD:EE:FF", 1)
    assert n == 1300
    assert [len(c) for c in sock.sent[:3]] == [512, 512, 276]
    assert sock.sent[3] == b"\x1b!?\r\n"
    assert st.ok
    assert t.is_connected and t.keep_alive


def test_channel_override(p21):
    sock = FakeSocket()
    t = transport(p21, sock, channel=3)
    run(t.connect())
    assert sock.address[1] == 3


def test_host_down_is_offline_with_reason(p21):
    t = transport(p21, FakeSocket(connect_error=OSError(errno.EHOSTDOWN, "Host is down")))
    with pytest.raises(TransportError) as e:
        run(t.connect())
    assert e.value.code == "offline"
    assert "off or out of range" in e.value.message
    assert not t.is_connected


def test_send_error_is_write_failed_and_drops_link(p21):
    sock = FakeSocket(send_error=OSError(errno.ECONNRESET, "reset"))
    t = transport(p21, sock)

    async def go():
        await t.connect()
        await t.write(bytes(2000))

    with pytest.raises(TransportError) as e:
        run(go())
    assert e.value.code == "write_failed"
    assert "512 of 2000" in e.value.message
    assert sock.closed and not t.is_connected


def test_send_timeout_is_timeout(p21):
    sock = FakeSocket(send_error=TimeoutError())
    t = transport(p21, sock)

    async def go():
        await t.connect()
        await t.write(bytes(2000))

    with pytest.raises(TransportError) as e:
        run(go())
    assert e.value.code == "timeout"


def test_peer_closed_is_offline(p21):
    sock = FakeSocket(replies=[b""])
    t = transport(p21, sock)

    async def go():
        await t.connect()
        await t.read(0.1)

    with pytest.raises(TransportError) as e:
        run(go())
    assert e.value.code == "offline"
    assert not t.is_connected


def test_no_bluetooth_sockets(monkeypatch, p21):
    monkeypatch.delattr(socket, "AF_BLUETOOTH", raising=False)
    t = RfcommTransport("aa:bb:cc:dd:ee:ff", p21.spp)
    with pytest.raises(TransportError) as e:
        run(t.connect())
    assert e.value.code == "offline"
    assert "RFCOMM" in e.value.message
    assert rfcomm.default_socket_factory is not None
