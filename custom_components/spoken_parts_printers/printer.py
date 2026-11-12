"""Runtime for one configured printer. No Home Assistant imports.

Holds the printer's state, validates jobs, makes sure jobs never interleave
(one asyncio lock per printer), and talks to the printer through a driver
(bytes) and a transport (link).

A job answers {"ok": True, "bytes": n, "pages": k} only after every byte was
written without error and, when the driver can read status, the printer
reported no error after the print. Anything else answers
{"ok": False, "code": <contract code>, "message": <plain words>, "bytes": n}.
"""

from __future__ import annotations

import asyncio
import base64
import binascii
import logging
import math
import time
from collections.abc import Callable
from datetime import UTC, datetime
from typing import Any

from .drivers.base import (
    BAD_FORMAT,
    BUSY,
    OFFLINE,
    TIMEOUT,
    TOO_BIG,
    WRITE_FAILED,
    WRONG_MEDIA,
    BatteryStatus,
    Driver,
    Page,
    PrinterStatus,
    PrintJob,
)
from .transports.base import Transport, TransportError, TransportTimeout

_LOGGER = logging.getLogger(__name__)

WAITING = "waiting"
READY = "ready"
PRINTING = "printing"
OFFLINE_STATE = "offline"
ERROR = "error"
STATES = (READY, PRINTING, OFFLINE_STATE, ERROR, WAITING)

MAX_PAGES = 20
MAX_COPIES = 50
MAX_JOB_BYTES = 4 * 1024 * 1024
MAX_CONTINUOUS_MM = 1000
# Allow a few dots over the label length for rounding on the backend side.
LENGTH_SLACK_DOTS = 8


def _utc_now() -> datetime:
    return datetime.now(UTC)


def iso_z(dt: datetime | None) -> str | None:
    if dt is None:
        return None
    return dt.astimezone(UTC).strftime("%Y-%m-%dT%H:%M:%SZ")


def fail(code: str, message: str, nbytes: int = 0) -> dict[str, Any]:
    return {"ok": False, "code": code, "message": message[:200], "bytes": nbytes}


class _JobFailed(Exception):
    def __init__(self, result: dict[str, Any], state: str) -> None:
        super().__init__(result["message"])
        self.result = result
        self.state = state


class PrinterRuntime:
    def __init__(
        self,
        *,
        printer_id: str,
        name: str,
        driver: Driver,
        transport: Transport,
        address: str,
        media_id: str,
        now: Callable[[], datetime] = _utc_now,
        job_timeout: float = 55.0,
        status_timeout: float = 2.0,
        busy_wait: float = 10.0,
        post_print_wait: float = 20.0,
        poll_interval: float = 0.5,
    ) -> None:
        self.printer_id = printer_id
        self.name = name
        self.driver = driver
        self.transport = transport
        self.address = address.upper()
        self.media_id = media_id
        self._now = now
        self.job_timeout = job_timeout
        self.status_timeout = status_timeout
        self.busy_wait = busy_wait
        self.post_print_wait = post_print_wait
        self.poll_interval = poll_interval

        self.state = WAITING
        self.last_error = ""
        self.last_error_code = ""
        self.battery: int | None = None
        self.charging: bool | None = None
        self.last_seen: datetime | None = None
        self.last_print: dict[str, Any] | None = None
        self._lock = asyncio.Lock()
        self._listeners: list[Callable[[], None]] = []

    # ----- listeners (Home Assistant entities subscribe here) -----

    def add_listener(self, cb: Callable[[], None]) -> Callable[[], None]:
        self._listeners.append(cb)

        def remove() -> None:
            if cb in self._listeners:
                self._listeners.remove(cb)

        return remove

    def _changed(self) -> None:
        for cb in list(self._listeners):
            try:
                cb()
            except Exception:  # noqa: BLE001 (a listener must not break a job)
                _LOGGER.exception("printer listener failed")

    def _set(self, state: str, code: str = "", message: str = "") -> None:
        self.state = state
        self.last_error_code = code
        self.last_error = message[:200]
        self._changed()

    def _seen(self) -> None:
        self.last_seen = self._now()

    @property
    def busy(self) -> bool:
        return self._lock.locked()

    def as_dict(self) -> dict[str, Any]:
        """The contract's printer object (list_printers and the link report)."""
        return {
            "id": self.printer_id,
            "name": self.name,
            "driver": self.driver.id,
            "transport": self.transport.name,
            "address": self.address,
            "state": self.state,
            "last_error": self.last_error,
            "battery": self.battery,
            "media": self.media_id,
            "last_seen": iso_z(self.last_seen),
        }

    # ----- validation -----

    def validate(self, call: dict[str, Any]) -> PrintJob | dict[str, Any]:
        """Return a PrintJob, or a failure dict for a job this printer cannot print."""
        d = self.driver
        caps = d.capabilities
        driver_id = call.get("driver")
        if driver_id != d.id:
            return fail(BAD_FORMAT, f"This printer uses the {d.id} driver, the job is for {driver_id}")
        fmt = call.get("format") or "raster"
        if fmt not in caps.formats:
            return fail(BAD_FORMAT, f"The {d.id} driver cannot print {fmt}")
        media_in = call.get("media")
        if media_in is None:
            # No media in the job: print on the labels this printer has loaded.
            media_id = self.media_id
        else:
            media_id = media_in.get("id") if isinstance(media_in, dict) else media_in
        if not isinstance(media_id, str) or not media_id:
            return fail(BAD_FORMAT, "The job has no media id")
        if media_id.lower() != self.media_id.lower():
            return fail(
                WRONG_MEDIA,
                f"The printer has {self.media_id} labels loaded, the job is for {media_id}",
            )
        media = d.media_for(media_id)
        if media is None:
            return fail(BAD_FORMAT, f"The {d.id} driver cannot print on {media_id} labels")
        copies = call.get("copies", 1)
        if isinstance(copies, bool) or not isinstance(copies, int) or copies < 1:
            return fail(BAD_FORMAT, "copies must be a whole number of at least 1")
        if copies > MAX_COPIES:
            return fail(TOO_BIG, f"At most {MAX_COPIES} copies per job")
        pages_in = call.get("pages")
        if not isinstance(pages_in, list) or not pages_in:
            return fail(BAD_FORMAT, "The job has no pages")
        if len(pages_in) > MAX_PAGES:
            return fail(TOO_BIG, f"At most {MAX_PAGES} pages per job")
        b64_budget = math.ceil(MAX_JOB_BYTES / 3) * 4
        pages: list[Page] = []
        total = 0
        for n, p in enumerate(pages_in, start=1):
            if not isinstance(p, dict):
                return fail(BAD_FORMAT, f"Page {n} is not an object")
            w, h, b64 = p.get("width"), p.get("height"), p.get("data_b64")
            if not all(isinstance(v, int) and not isinstance(v, bool) and v > 0 for v in (w, h)):
                return fail(BAD_FORMAT, f"Page {n} needs a positive width and height")
            if not isinstance(b64, str) or not b64:
                return fail(BAD_FORMAT, f"Page {n} has no data")
            b64_budget -= len(b64)
            if b64_budget < 0:
                return fail(TOO_BIG, f"The job is larger than {MAX_JOB_BYTES // 1024} KiB")
            if fmt == "raster":
                if w > caps.print_width_dots:
                    return fail(
                        TOO_BIG,
                        f"Page {n} is {w} dots wide, the printer prints {caps.print_width_dots}",
                    )
                if media.length_mm is not None:
                    limit = round(media.length_mm * caps.dpi / 25.4) + LENGTH_SLACK_DOTS
                else:
                    limit = round(MAX_CONTINUOUS_MM * caps.dpi / 25.4)
                if h > limit:
                    return fail(TOO_BIG, f"Page {n} is {h} dots long, the label fits {limit}")
            try:
                data = base64.b64decode(b64, validate=True)
            except (binascii.Error, ValueError):
                return fail(BAD_FORMAT, f"Page {n} data is not valid base64")
            if fmt == "raster":
                want = math.ceil(w / 8) * h
                if len(data) != want:
                    return fail(
                        BAD_FORMAT,
                        f"Page {n} data is {len(data)} bytes, a {w}x{h} raster needs {want}",
                    )
            elif not data:
                return fail(BAD_FORMAT, f"Page {n} has no data")
            total += len(data)
            if total > MAX_JOB_BYTES:
                return fail(TOO_BIG, f"The job is larger than {MAX_JOB_BYTES // 1024} KiB")
            pages.append(Page(width=w, height=h, data=data))
        job_id = call.get("job_id") or call.get("id") or ""
        return PrintJob(
            format=fmt, media=media, copies=copies, pages=tuple(pages), job_id=str(job_id)
        )

    # ----- status -----

    async def _query_status(self) -> PrinterStatus | None:
        cmd = self.driver.status_query()
        if cmd is None:
            return None
        last: TransportTimeout | None = None
        for _ in range(2):
            try:
                st = await self.transport.request(cmd, self.driver.parse_status, self.status_timeout)
            except TransportTimeout as err:
                last = err
                continue
            self._seen()
            return st
        assert last is not None
        raise last

    async def _query_battery(self) -> None:
        cmd = self.driver.battery_query()
        if cmd is None:
            return
        try:
            b: BatteryStatus = await self.transport.request(
                cmd, self.driver.parse_battery, self.status_timeout
            )
        except (TransportError, ValueError):
            return  # battery is nice to have; never fail a job for it
        self._seen()
        self.battery = b.level
        self.charging = b.charging

    async def _wait_until_idle(self, limit: float) -> PrinterStatus | None:
        """Poll status until the printer is not busy or limit seconds pass."""
        deadline = time.monotonic() + limit
        st = await self._query_status()
        while st is not None and st.busy and time.monotonic() < deadline:
            await asyncio.sleep(self.poll_interval)
            st = await self._query_status()
        return st

    # ----- printing -----

    async def print_raster(self, call: dict[str, Any]) -> dict[str, Any]:
        job = self.validate(call)
        if isinstance(job, dict):
            self._note_print(call.get("job_id"), job)
            return job
        started = False
        try:
            async with asyncio.timeout(self.job_timeout):
                async with self._lock:
                    started = True
                    result = await self._print_locked(job)
        except TimeoutError:
            if not started:
                result = fail(BUSY, "Another job is still printing on this printer")
            else:
                await self.transport.close()
                result = fail(TIMEOUT, "The printer did not finish the job in time")
                self._set(ERROR, TIMEOUT, result["message"])
        self._note_print(job.job_id, result)
        return result

    def _note_print(self, job_id: Any, result: dict[str, Any]) -> None:
        self.last_print = {
            "job_id": str(job_id or ""),
            "at": iso_z(self._now()),
            "ok": result["ok"],
            "code": result.get("code", ""),
            "bytes": result.get("bytes", 0),
        }
        self._changed()

    async def _print_locked(self, job: PrintJob) -> dict[str, Any]:
        self._set(PRINTING, self.last_error_code, self.last_error)
        written = 0
        try:
            try:
                await self.transport.connect()
            except TransportError as err:
                raise _JobFailed(fail(OFFLINE, err.message), OFFLINE_STATE) from None
            self._seen()

            # Before: do not waste a label on a printer that cannot print.
            try:
                st = await self._wait_until_idle(self.busy_wait)
            except TransportError as err:
                await self.transport.close()
                raise _JobFailed(
                    fail(TIMEOUT if err.code == TIMEOUT else err.code,
                         f"The printer did not answer before printing: {err.message}"),
                    OFFLINE_STATE,
                ) from None
            if st is not None and not st.ok:
                code = BUSY if st.busy else st.code
                raise _JobFailed(fail(code, st.message), ERROR if not st.busy else READY)

            try:
                data = self.driver.encode(job)
            except ValueError as err:
                raise _JobFailed(fail(BAD_FORMAT, str(err)), READY) from None

            try:
                written = await self.transport.write(data)
            except TransportError as err:
                await self.transport.close()
                code = err.code if err.code in (WRITE_FAILED, TIMEOUT) else WRITE_FAILED
                raise _JobFailed(fail(code, err.message), ERROR) from None
            if written != len(data):
                await self.transport.close()
                raise _JobFailed(
                    fail(WRITE_FAILED, f"Only {written} of {len(data)} bytes reached the printer", written),
                    ERROR,
                )
            self._seen()

            # After: the label only counts as printed if the printer says no error.
            try:
                st = await self._wait_until_idle(self.post_print_wait)
            except TransportError as err:
                await self.transport.close()
                raise _JobFailed(
                    fail(TIMEOUT if err.code == TIMEOUT else err.code,
                         "The label was sent but the printer did not confirm it", written),
                    ERROR,
                ) from None
            if st is not None and not st.ok and not st.busy:
                raise _JobFailed(fail(st.code, st.message, written), ERROR)

            await self._query_battery()
            self._set(READY)
            return {"ok": True, "bytes": written, "pages": len(job.pages)}
        except _JobFailed as jf:
            result = jf.result
            result["bytes"] = max(result.get("bytes", 0), written)
            if jf.state == READY:
                self._set(READY, self.last_error_code, self.last_error)
            else:
                self._set(jf.state, result["code"], result["message"])
            return result
        finally:
            if not self.transport.keep_alive:
                await self.transport.close()

    # ----- periodic refresh -----

    async def refresh(self) -> None:
        """Read status and battery. Keeps a keep_alive link warm.

        A transport that does not keep its link open is not opened just to
        look (the P21 beeps on every new link); its state comes from jobs.
        """
        if self._lock.locked():
            return
        async with self._lock:
            if not self.transport.keep_alive and not self.transport.is_connected:
                return
            try:
                await self.transport.connect()
            except TransportError as err:
                self._set(OFFLINE_STATE, OFFLINE, err.message)
                return
            self._seen()
            try:
                st = await self._query_status()
                await self._query_battery()
            except TransportError as err:
                await self.transport.close()
                self._set(OFFLINE_STATE, OFFLINE, f"The printer did not answer: {err.message}")
                return
            if st is None or st.ok or st.busy:
                self._set(READY)
            else:
                self._set(ERROR, st.code, st.message)

    async def close(self) -> None:
        await self.transport.close()
