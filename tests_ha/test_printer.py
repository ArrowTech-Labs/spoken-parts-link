import asyncio
import base64

import pytest
from conftest import run
from helpers import P21Printer, make_runtime, p21_call, page
from test_registry import Fake4x6

from custom_components.spoken_parts_printers.drivers import registry
from custom_components.spoken_parts_printers.transports.fake import FakeTransport


def p21_runtime(p21, printer=None, **kw):
    t = FakeTransport()
    t.responder = printer or P21Printer()
    return make_runtime(p21, t, **kw), t


def test_ok_path(p21):
    async def go():
        rt, t = p21_runtime(p21)
        res = await rt.print_raster(p21_call())
        return rt, t, res

    rt, t, res = run(go())
    job_bytes = [w for w in t.writes if w.startswith(b"SIZE")]
    assert len(job_bytes) == 1
    assert res == {"ok": True, "bytes": len(job_bytes[0]), "pages": 1}
    assert rt.state == "ready" and rt.last_error == ""
    assert rt.battery == 85 and rt.charging is False
    assert rt.last_seen is not None
    assert rt.last_print["ok"] is True and rt.last_print["job_id"] == "job-1"
    # Order: status before, the job, status after, battery.
    assert t.responder.queries == ["status", "status", "battery"]
    assert t.connected  # spp keeps the link warm


def test_as_dict_shape(p21):
    rt, _ = p21_runtime(p21)
    d = rt.as_dict()
    assert set(d) == {"id", "name", "driver", "transport", "address", "state",
                      "last_error", "battery", "media", "last_seen"}
    assert d["state"] == "waiting" and d["address"] == "AA:BB:CC:DD:EE:FF"
    assert d["driver"] == "nelko.p21" and d["media"] == "14x40" and d["last_seen"] is None


def test_write_failure(p21):
    rt, t = p21_runtime(p21)
    t.fail_write_after = 100
    res = run(rt.print_raster(p21_call()))
    assert res["ok"] is False and res["code"] == "write_failed"
    assert res["bytes"] == 0 or res["bytes"] <= 100
    assert rt.state == "error" and rt.last_error_code == "write_failed"
    assert not t.connected


def test_write_timeout(p21):
    rt, t = p21_runtime(p21)
    t.timeout_write = True
    res = run(rt.print_raster(p21_call()))
    assert (res["ok"], res["code"]) == (False, "timeout")


def test_offline(p21):
    rt, t = p21_runtime(p21)
    t.fail_connect = True
    res = run(rt.print_raster(p21_call()))
    assert (res["ok"], res["code"]) == (False, "offline")
    assert "Bluetooth" in res["message"]
    assert rt.state == "offline"
    assert t.written == b""


@pytest.mark.parametrize(
    ("over", "code"),
    [
        ({"driver": "nelko.pm220"}, "bad_format"),
        ({"format": "pdf"}, "bad_format"),
        ({"media": {"id": "14x50"}}, "wrong_media"),
        ({"media": {}}, "bad_format"),
        ({"pages": [{"width": 96, "height": 320, "data_b64": "not base64!!"}]}, "bad_format"),
        ({"pages": [{"width": 96, "height": 320, "data_b64": base64.b64encode(b"\0" * 10).decode()}]}, "bad_format"),
        ({"pages": [page(104, 320)]}, "too_big"),
        ({"pages": [page(96, 400)]}, "too_big"),
        ({"pages": [page(96, 8)] * 21}, "too_big"),
        ({"pages": []}, "bad_format"),
        ({"pages": [{"width": 0, "height": 8, "data_b64": "AA=="}]}, "bad_format"),
        ({"copies": 51}, "too_big"),
        ({"copies": 0}, "bad_format"),
    ],
)
def test_bad_payloads(p21, over, code):
    rt, t = p21_runtime(p21)
    res = run(rt.print_raster(p21_call(**over)))
    assert res["ok"] is False and res["code"] == code, res
    assert t.written == b"" and t.connects == 0
    assert rt.state == "waiting"


def test_missing_media_uses_loaded_labels(p21):
    call = p21_call()
    del call["media"]
    rt, t = p21_runtime(p21)
    res = run(rt.print_raster(call))
    assert res["ok"] is True
    assert any(w.startswith(b"SIZE 14.0 mm,40.0 mm") for w in t.writes)


def test_narrower_page_and_slack_ok(p21):
    rt, _ = p21_runtime(p21)
    res = run(rt.print_raster(p21_call(pages=[page(90, 328)])))
    assert res["ok"] is True


def test_no_interleaving_under_concurrent_jobs(p21):
    class Recording(FakeTransport):
        inflight = 0
        max_inflight = 0

        async def write(self, data):
            Recording.inflight += 1
            Recording.max_inflight = max(Recording.max_inflight, Recording.inflight)
            try:
                await asyncio.sleep(0.01)
                return await super().write(data)
            finally:
                Recording.inflight -= 1

    async def go():
        t = Recording()
        t.responder = P21Printer()
        rt = make_runtime(p21, t)
        calls = [p21_call(job_id=f"j{i}", copies=i + 1) for i in range(4)]
        results = await asyncio.gather(*(rt.print_raster(c) for c in calls))
        return t, results

    t, results = run(go())
    assert all(r["ok"] for r in results)
    assert Recording.max_inflight == 1
    jobs = [w for w in t.writes if w.startswith(b"SIZE")]
    assert len(jobs) == 4
    # Every job went out in one piece, each followed by its own status checks.
    kinds = [b"J" if w.startswith(b"SIZE") else w[:1] for w in t.writes]
    assert kinds == [b"\x1b", b"J", b"\x1b", b"B"] * 4


def test_status_error_after_print_is_not_ok(p21):
    rt, t = p21_runtime(p21, P21Printer(statuses=[0x00], after_job=[0x04]))
    res = run(rt.print_raster(p21_call()))
    assert res["ok"] is False and res["code"] == "no_media"
    assert res["bytes"] > 3840
    assert rt.state == "error" and rt.last_error == "No labels loaded"


def test_busy_after_print_then_ready_is_ok(p21):
    rt, t = p21_runtime(p21, P21Printer(statuses=[0x00], after_job=[0x20, 0x02, 0x00]))
    res = run(rt.print_raster(p21_call()))
    assert res["ok"] is True
    assert t.responder.queries.count("status") == 4


def test_no_status_reply_after_print_is_timeout(p21):
    rt, t = p21_runtime(p21, P21Printer(silent_after_job=True))
    res = run(rt.print_raster(p21_call()))
    assert res["ok"] is False and res["code"] == "timeout"
    assert res["bytes"] > 0


def test_cover_open_before_print_sends_no_job(p21):
    rt, t = p21_runtime(p21, P21Printer(statuses=[0x01]))
    res = run(rt.print_raster(p21_call()))
    assert res["code"] == "cover_open"
    assert not any(w.startswith(b"SIZE") for w in t.writes)
    assert rt.state == "error"


def test_still_busy_before_print(p21):
    rt, t = p21_runtime(p21, P21Printer(statuses=[0x20]))
    res = run(rt.print_raster(p21_call()))
    assert res["code"] == "busy"
    assert not any(w.startswith(b"SIZE") for w in t.writes)


def test_pm220_without_status_ok_after_write(pm220):
    t = FakeTransport()
    rt = make_runtime(pm220, t)
    call = p21_call(driver="nelko.pm220", media={"id": "50x30"}, pages=[page(384, 240)])
    res = run(rt.print_raster(call))
    assert res["ok"] is True
    assert t.writes and t.writes[0].startswith(b"SIZE 50 mm,30 mm")
    assert rt.battery is None


def test_job_timeout(p21):
    rt, t = p21_runtime(p21, job_timeout=0.05)
    t.write_delay = 0.5
    res = run(rt.print_raster(p21_call()))
    assert res["code"] == "timeout"
    assert rt.state == "error"


def test_waiting_for_lock_too_long_is_busy(p21):
    async def go():
        rt, t = p21_runtime(p21, job_timeout=0.1)
        await rt._lock.acquire()
        return await rt.print_raster(p21_call())

    assert run(go())["code"] == "busy"


def test_closes_link_when_not_kept_alive(p21):
    t = FakeTransport(keep_alive=False)
    t.responder = P21Printer()
    rt = make_runtime(p21, t)
    assert run(rt.print_raster(p21_call()))["ok"]
    assert not t.connected and t.closes == 1


def test_refresh(p21):
    rt, t = p21_runtime(p21, P21Printer(statuses=[0x00], battery=b"\x42\x01"))
    run(rt.refresh())
    assert rt.state == "ready" and rt.battery == 42 and rt.charging is True
    assert t.connected


def test_refresh_reports_error_and_offline(p21):
    rt, t = p21_runtime(p21, P21Printer(statuses=[0x01]))
    run(rt.refresh())
    assert rt.state == "error" and rt.last_error_code == "cover_open"
    rt2, t2 = p21_runtime(p21)
    t2.fail_connect = True
    run(rt2.refresh())
    assert rt2.state == "offline"


def test_refresh_does_not_open_a_link_it_would_not_keep(p21):
    t = FakeTransport(keep_alive=False)
    rt = make_runtime(p21, t)
    run(rt.refresh())
    assert t.connects == 0 and rt.state == "waiting"


def test_listeners_called(p21):
    rt, _ = p21_runtime(p21)
    seen = []
    remove = rt.add_listener(lambda: seen.append(rt.state))
    run(rt.print_raster(p21_call()))
    assert "printing" in seen and seen[-1] == "ready"
    remove()
    n = len(seen)
    run(rt.refresh())
    assert len(seen) == n


def test_other_format_driver_through_runtime():
    d = registry.register(Fake4x6())
    try:
        t = FakeTransport()
        rt = make_runtime(d, t)
        zpl = b"^XA^FO50,50^FDHello^FS^XZ"
        call = {
            "printer_id": rt.printer_id, "job_id": "z1", "driver": d.id, "format": "zpl",
            "media": {"id": "102x152"}, "copies": 2,
            "pages": [{"width": 812, "height": 1218, "data_b64": base64.b64encode(zpl).decode()}],
        }
        res = run(rt.print_raster(call))
        assert res == {"ok": True, "bytes": len(zpl) * 2, "pages": 1}
        assert bytes(t.written) == zpl * 2
        call["format"] = "raster"
        assert run(rt.print_raster(call))["code"] == "bad_format"
    finally:
        registry.unregister(d.id)
