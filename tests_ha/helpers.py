"""Shared test helpers."""

import base64
import math

from custom_components.spoken_parts_printers.printer import PrinterRuntime
from custom_components.spoken_parts_printers.transports.fake import FakeTransport


def page(width, height, fill=0x00):
    data = bytes([fill]) * (math.ceil(width / 8) * height)
    return {"width": width, "height": height, "data_b64": base64.b64encode(data).decode()}


def p21_call(**over):
    call = {
        "printer_id": "nelko.p21:aa:bb:cc:dd:ee:ff",
        "job_id": "job-1",
        "driver": "nelko.p21",
        "document_type": "spool_label",
        "media": {"id": "14x40", "kind": "die_cut", "width_mm": 14, "length_mm": 40},
        "copies": 1,
        "pages": [page(96, 320)],
    }
    call.update(over)
    return call


class P21Printer:
    """Scripted P21 replies for FakeTransport.responder.

    statuses: status bytes to answer, in order; the last one repeats.
    After the job bytes are written, after_job (if set) replaces them.
    """

    def __init__(self, statuses=(0x00,), after_job=None, battery=b"\x85\x00", silent_after_job=False):
        self.statuses = list(statuses)
        self.after_job = list(after_job) if after_job is not None else None
        self.battery = battery
        self.silent_after_job = silent_after_job
        self.job_seen = False
        self.queries = []

    def __call__(self, data):
        if data.startswith(b"\x1b!?"):
            self.queries.append("status")
            if self.job_seen and self.silent_after_job:
                return None
            seq = self.after_job if (self.job_seen and self.after_job is not None) else self.statuses
            value = seq.pop(0) if len(seq) > 1 else seq[0]
            return bytes([value])
        if data.startswith(b"BATTERY?"):
            self.queries.append("battery")
            return b"BATTERY " + self.battery + b"\r\n"
        if b"PRINT " in data:
            self.job_seen = True
        return None


def make_runtime(driver, transport=None, media_id=None, **kw):
    transport = transport or FakeTransport()
    kw.setdefault("status_timeout", 0.2)
    kw.setdefault("poll_interval", 0.01)
    kw.setdefault("busy_wait", 0.3)
    kw.setdefault("post_print_wait", 0.3)
    return PrinterRuntime(
        printer_id=f"{driver.id}:aa:bb:cc:dd:ee:ff",
        name="Label maker",
        driver=driver,
        transport=transport,
        address="aa:bb:cc:dd:ee:ff",
        media_id=media_id or driver.capabilities.default_media,
        **kw,
    )
