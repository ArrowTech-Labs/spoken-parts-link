"""The Spoken Parts power engine: single-shot shed, lockout and restore, per circuit.

Plain Python with no Home Assistant imports, so the same code runs in Home
Assistant (integration.py) and in the tests. It is the product version of the
design's reference model (ATS-SP-POWER-SHED-DESIGN-1, proposal/sim/shed_model.py)
with Marc's answers applied (Nov 15, 2026):

- No shadow mode and no levels. A circuit with protect on and at least one plug
  protects at once: heaters and dryers, then printers that are not printing, then
  printing printers by the least deposited value.
- 4C: heaters and dryers, then idle printers, come back by themselves. A printer
  that was printing (or starting) when it was cut stays off until a person asks,
  and the engine checks there is room first.

After the two wrong H2D cuts of Nov 15, 2026 (ATS-SP-POWER-INCIDENT-1 and
ATS-SP-POWER-FIX-1), with Marc's fixes A, B and C:

- A. Before any printer is cut, every plug on the circuit is read again and the
  decision uses fresh readings only: no reading older than the plugs' reporting
  interval (`fresh_s`, 5 s) counts. A printer is cut only if the circuit is
  still over the shed line on those fresh numbers.
- B. Heaters and dryers keep the fast trigger (the circuit's sustain_s mean).
  A printer may be cut only when the circuit has been over the shed line
  continuously for `printer_sustain_s` (60 s), or is over the hard line on
  fresh readings. This is an engine constant, not a circuit setting.
- C. A heater or dryer in an active heating session (it drew at least
  `restart_dry_w` within `heater_session_s`) counts at its learned draw in the
  line-up, even in the off half of its cycle, so it goes before any printer.

Time is seconds on a monotonic clock (`t`). Money is in mills (tenths of a cent).
The engine never talks to Home Assistant itself: it calls the actuator
(`switch(key, action, t)` and `read(key, t) -> bool`) and is told what happened
(`on_power`, `on_relay`, `on_command`, `on_reachable`, `on_feed`).

The seven rules (DESIGN 6.1) are marked "rule n" where they are enforced.
"""

from __future__ import annotations

import math
from dataclasses import dataclass, field
from typing import Any, Callable, Protocol

HEATERS = ("ams_ht", "dryer")
ROLES = ("printer", "ams_ht", "dryer", "other")
PRINTER_STATES = (
    "idle", "finished", "failed", "starting", "printing", "paused", "updating", "unknown",
)
NOT_PRINTING = ("idle", "finished", "failed")
ACTIVE_STATES = ("WATCH", "HOLD", "RESTORE", "OVER")
STATES = ("OFF", "STARTING", "WATCH", "SHED", "LOCKOUT", "HOLD", "RESTORE", "OVER", "BLIND")


class Actuator(Protocol):
    def switch(self, key: str, action: str, t: float) -> None: ...

    def read(self, key: str, t: float) -> bool: ...


@dataclass
class Timers:
    """DESIGN 6 to 8 and EVENT-LIFECYCLE 4. Seconds and watts."""

    hard_confirm_s: float = 0.25
    # Fix A: the plugs report about every 5 s under load. Older readings never
    # count when a printer may be cut (they were 10 s before Nov 15, 2026).
    fresh_s: float = 5.0
    # Fix A: wait this long at most for every plug's fresh re-read.
    refresh_timeout_s: float = 2.0
    # Fix B: over the shed line continuously this long before a printer may be
    # cut (the hard line still acts at once). Heaters keep the sustain_s filter.
    printer_sustain_s: float = 60.0
    # Fix C: a heater that drew restart_dry_w within this long is still in its
    # heating session, so it counts at its learned draw between pulses.
    heater_session_s: float = 180.0
    printer_wait_log_s: float = 300.0  # at most one printer_wait entry this often
    lockout_min_s: float = 3.0
    lockout_max_s: float = 10.0
    verify_off_s: float = 5.0
    off_w: float = 5.0
    min_useful_w: float = 5.0
    max_rounds: int = 3
    restore_hold_s: float = 120.0
    restore_settle_s: float = 90.0
    restore_tries: int = 3
    reshed_window_s: float = 1800.0
    max_hold_s: float = 1800.0
    feed_stale_s: float = 15.0
    feed_dead_s: float = 120.0
    heartbeat_s: float = 30.0  # no report this long: read the plug directly
    silent_s: float = 90.0  # no report this long: the plug is not answering
    start_timeout_s: float = 60.0
    predict_seed_s: float = 600.0
    predict_cap_s: float = 1200.0
    max_plug_w: float = 2100.0
    restart_dry_s: float = 90.0
    restart_dry_w: float = 15.0
    room_window_s: float = 120.0
    all_clear_s: float = 60.0
    idle_printer_w: float = 60.0
    margin_pct: float = 0.05


@dataclass
class CircuitCfg:
    id: str
    name: str = "Circuit"
    protect: bool = True
    breaker_amps: int = 15
    volts: int = 120
    shed_w: float = 1440.0
    restore_w: float = 1080.0
    hard_w: float = 1800.0
    unmetered_w: float = 0.0
    sustain_s: float = 2.0

    @property
    def rating_w(self) -> float:
        return float(self.breaker_amps * self.volts)

    @property
    def margin_w(self) -> float:
        return round(self.shed_w * 0.05)

    @property
    def target_w(self) -> float:
        return self.shed_w - self.margin_w


def validate_circuit(d: dict[str, Any]) -> tuple[CircuitCfg | None, list[str]]:
    """Guard rails (DESIGN 3, item 3). The engine refuses a circuit outside them."""
    errors: list[str] = []
    try:
        c = CircuitCfg(
            id=str(d["id"]),
            name=str(d.get("name") or "Circuit")[:60],
            protect=bool(d.get("protect", True)),
            breaker_amps=int(d.get("breaker_amps", 15)),
            volts=int(d.get("volts", 120)),
            shed_w=float(d["shed_w"]),
            restore_w=float(d["restore_w"]),
            hard_w=float(d["hard_w"]),
            unmetered_w=float(d.get("unmetered_w") or 0),
            sustain_s=float(d.get("sustain_s", 2.0)),
        )
    except (KeyError, TypeError, ValueError):
        return None, [f"circuit {d.get('id')!s}: missing or bad fields"]
    r = c.rating_w
    if not 1 <= c.breaker_amps <= 100 or not 100 <= c.volts <= 250:
        errors.append(f"{c.name}: breaker amps or volts out of range")
    if not 0.4 * r - 0.5 <= c.shed_w <= r + 0.5:
        errors.append(f"{c.name}: the shed line must be 40 to 100 percent of the breaker")
    if c.restore_w >= c.shed_w or c.restore_w <= 0:
        errors.append(f"{c.name}: the restore line must be under the shed line")
    if not c.shed_w < c.hard_w <= r + 0.5:
        errors.append(f"{c.name}: the hard line must be above the shed line and at most the breaker")
    if not 0 <= c.unmetered_w < c.shed_w:
        errors.append(f"{c.name}: unmetered watts must be under the shed line")
    if not 0 <= c.sustain_s <= 10:
        errors.append(f"{c.name}: the trip filter must be 0 to 10 seconds")
    return (None if errors else c), errors


@dataclass
class Feed:
    """What the link last said about one printer (DESIGN 4)."""

    state: str = "unknown"
    value: int | None = None  # deposited value in mills; None = not known
    basis: str = "none"
    job: str = ""
    at: float = -1e9
    high_until: float | None = None  # predicted end of the heat-up window
    start_t: float | None = None
    start_peak: float = 0.0
    low_since: float | None = None
    confirmed: bool = True


def p_n(values: list[float], pct: float) -> float:
    if not values:
        return 0.0
    s = sorted(values)
    i = min(len(s) - 1, max(0, math.ceil(pct * len(s)) - 1))
    return s[i]


@dataclass
class Plug:
    key: str
    role: str = "other"
    eligible: bool = True  # may_turn_off
    peak_w: float = 0.0  # rated or typed draw (the floor for the expected draw)
    circuit: str | None = None
    label: str = ""
    controllable: bool = True  # a THIRDREALITY 3RSP02064Z on ZHA (checked by the glue)
    printer_serial: str | None = None
    w: float = 0.0
    at: float = -1e9  # when w was read
    relay: str = "on"
    available: bool = True
    silent: bool = False
    zeroed: bool = False
    fired_at: float | None = None
    confirmed_at: float | None = None
    retried: bool = False
    verify_sent: bool = False
    failed_open: bool = False
    booked_w: float = 0.0
    booked_until: float = -1e9
    shed_kind: str | None = None  # heater, idle, sacrifice (while the engine holds it off)
    shed_at: float = 0.0
    shed_w: float = 0.0
    shed_value: int | None = None
    shed_job: str = ""
    tries: int = 0
    hold_mult: int = 1
    restored_at: float = -1e9
    gave_up: bool = False
    blocked_logged: bool = False
    pending: str | None = None  # "on" or "off" while a command is out
    suspect_at: float | None = None  # an off plug reported power: re-read before believing it
    suspect_reread: bool = False
    unavailable_at: float = -1e9
    pending_at: float = -1e9
    person_on: bool = False
    read_at: float = -1e9
    dry_check_at: float | None = None
    dry_was_w: float = 0.0
    drew_at: float = -1e9  # fix C: last reading of at least restart_dry_w while on
    stuck: bool = False
    hist: list[float] = field(default_factory=list)  # draws while on and drawing
    idle_hist: list[float] = field(default_factory=list)  # printer draws while idle
    gaps: list[float] = field(default_factory=list)  # gaps between reports
    starts: list[float] = field(default_factory=list)  # measured heat-up seconds
    feed: Feed = field(default_factory=Feed)

    @property
    def reachable(self) -> bool:
        return self.available and not self.silent

    @property
    def learned_w(self) -> float:
        return round(p_n(self.hist, 0.95), 1) if len(self.hist) >= 5 else 0.0

    def expected_w(self, tm: Timers) -> float:
        """What it will draw when it is turned back on (DESIGN 7)."""
        if self.role == "printer":
            idle = p_n(self.idle_hist, 0.95) if len(self.idle_hist) >= 5 else 0.0
            return max(self.shed_w if self.shed_kind == "idle" else 0.0, idle) or (
                self.peak_w or tm.idle_printer_w
            )
        return max(self.peak_w, self.shed_w, self.learned_w)


@dataclass
class Cand:
    plug: Plug
    tier: int
    key: tuple
    w: float
    value: int | None
    note: str


class Journal:
    """Audit entries with sequence numbers, kept until Spoken Parts has stored them."""

    def __init__(self, wall: Callable[[], str], cap: int = 5000) -> None:
        self.wall = wall
        self.cap = cap
        self.seq = 0
        self.items: list[dict[str, Any]] = []
        self.dropped = 0

    def add(self, t: float, kind: str, circuit: str | None, plug: str | None,
            event: int | None, data: dict[str, Any]) -> dict[str, Any]:
        self.seq += 1
        e = {"seq": self.seq, "kind": kind, "circuit": circuit, "plug": plug, "event": event,
             "at": self.wall(), "mono": round(t, 3), "data": data}
        self.items.append(e)
        if len(self.items) > self.cap:
            cut = len(self.items) - self.cap
            self.dropped += cut
            del self.items[:cut]
        return e

    def after(self, seq: int, limit: int = 200) -> list[dict[str, Any]]:
        return [e for e in self.items if e["seq"] > seq][:limit]

    def ack(self, through: int) -> int:
        self.items = [e for e in self.items if e["seq"] > through]
        return len(self.items)


class Circuit:
    """One breaker: its plugs, its sum and its state machine (EVENT-LIFECYCLE 1 and 2)."""

    def __init__(self, eng: Engine, cfg: CircuitCfg) -> None:
        self.eng = eng
        self.cfg = cfg
        self.tm = eng.tm
        self.plugs: dict[str, Plug] = {}
        self.state = "STARTING" if cfg.protect else "OFF"
        self.started_at = 0.0
        self.samples: list[tuple[float, float]] = []
        self.window_from = 0.0
        self.event = 0
        self.event_open = False
        self.opened_at = 0.0
        self.closed_at = -1e9
        self.peak = 0.0
        self.rounds = 0
        self.fired: list[Plug] = []
        self.lock_at = 0.0
        self.decide_at: float | None = None
        self.outcome = "ok"
        self.under_since: float | None = None
        self.hist: list[tuple[float, float]] = []
        self.restore_until = 0.0
        self.restoring: str | None = None
        self.hard_since: float | None = None
        self.over_logged = False
        self.blind_reason = ""
        self.pending: CircuitCfg | None = None
        self.drop_after = False
        self.switching_off_logged = False
        self.lost_print = False
        self.over_since: float | None = None  # fix B: over the shed line continuously since
        self.wait_logged = False  # fix B: one printer_wait entry per stretch over the line
        self.wait_log_at = -1e9
        # Fix A: after a decision that could not check some plugs, the printer path waits
        # until one of them has something new (a reading, or it went unreachable).
        self.wait_for: dict[str, tuple[float, bool]] = {}
        self.refresh_at: float | None = None  # fix A: when the pre-cut re-read went out
        self.refresh_keys: list[str] = []
        self.shot_tier = 3  # the highest tier this round's decision may cut
        self.shot_from = "WATCH"  # the state the printer path started from

    # ---- sums ---------------------------------------------------------------------
    @staticmethod
    def eff(p: Plug, t: float) -> float:
        if p.zeroed:
            return 0.0  # rule 2: fired means zero
        if t < p.booked_until:
            return max(p.w, p.booked_w)  # booked: the mirror of fired means zero
        return p.w

    def total(self, t: float, verified_only: bool = False) -> float:
        if verified_only:  # measured watts only: a booked (in-flight) restore is not a reading
            return self.cfg.unmetered_w + sum(
                0.0 if p.zeroed else p.w for p in self.plugs.values() if self._verified(p, t))
        return self.cfg.unmetered_w + sum(self.eff(p, t) for p in self.plugs.values())

    def _verified(self, p: Plug, t: float) -> bool:
        return p.zeroed or t - p.at <= self.tm.fresh_s

    def mean(self, t: float) -> float | None:
        s = self.cfg.sustain_s
        if s <= 0:
            return self.total(t)
        if t - self.window_from < s:
            return None
        lo, acc, prev_t, prev_v = t - s, 0.0, None, None
        for ts, v in self.samples:
            if prev_t is not None and ts > lo:
                acc += prev_v * (ts - max(prev_t, lo))
            prev_t, prev_v = ts, v
        if prev_t is not None:
            acc += prev_v * (t - max(prev_t, lo))
        return acc / s

    # ---- the ladder (kept current: the "finger on the trigger") -----------------------
    def ladder(self, t: float, max_tier: int = 3) -> tuple[list[Cand], list[tuple[str, str]]]:
        tm = self.tm
        out: list[Cand] = []
        skipped: list[tuple[str, str]] = []
        for p in self.plugs.values():
            w = self.eff(p, t)
            heating = p.role in HEATERS and self.in_session(p, t) and w < p.learned_w
            if heating:
                w = p.learned_w  # fix C: between pulses it counts at its learned draw
            why = None
            if p.role == "other" or not p.eligible:
                why = "not allowed to be turned off"
            elif not p.controllable:
                why = "not a plug the engine may switch"
            elif not self.eng.switching_allowed:
                why = "switching is turned off in Home Assistant"
            elif p.failed_open:
                why = "did not open earlier"
            elif p.relay != "on" or p.zeroed:
                why = "already off"
            elif not p.reachable:
                why = "plug not reachable"
            elif w < tm.min_useful_w:
                why = "not drawing"
            if why:
                skipped.append((p.key, why))
                continue
            if p.role in HEATERS:
                c = Cand(p, 1, (w, p.key), w, None,
                         "heater or dryer between heating pulses (counted at its learned draw)"
                         if heating else "heater or dryer that is drawing")
            else:
                f, age = p.feed, t - p.feed.at
                if age > tm.feed_dead_s or f.state in ("unknown", "updating"):
                    skipped.append((p.key, "firmware update running" if f.state == "updating"
                                    else "printer state not known"))
                    continue
                stale = " (feed %ds old)" % age if age > tm.feed_stale_s else ""
                if f.state in NOT_PRINTING:
                    if stale:  # never cut a printer as idle on an old feed
                        skipped.append((p.key, "printer state not known (feed %ds old)" % age))
                        continue
                    c = Cand(p, 2, (w, p.key), w, None, "printer not printing")
                elif f.state == "starting":
                    c = Cand(p, 3, (0, 0, -w, p.key), w, 0, "starting, nothing deposited" + stale)
                elif f.value is None:
                    c = Cand(p, 3, (2, 0, -w, p.key), w, None, "printing, value not known" + stale)
                else:
                    c = Cand(p, 3, (1, f.value, -w, p.key), w, f.value, "printing" + stale)
            if c.tier > max_tier:
                skipped.append((p.key, "no more prints are cut in this event"))
                continue
            out.append(c)
        out.sort(key=lambda c: (c.tier, c.key))
        return out, skipped

    def in_session(self, p: Plug, t: float) -> bool:
        """Fix C: a heater or dryer whose heating session is still going. It cycles
        (an AMS HT dries in pulses of about 150 W every 20 to 60 s), so a low reading
        between pulses does not mean it is done."""
        return (p.role in HEATERS and p.relay == "on" and not p.zeroed
                and p.learned_w >= self.tm.restart_dry_w
                and t - p.drew_at <= self.tm.heater_session_s)

    @staticmethod
    def _prefix(cands: list[Cand], need: float) -> tuple[list[Cand], bool]:
        pick: list[Cand] = []
        freed = 0.0
        for c in cands:
            if freed >= need:
                break
            pick.append(c)
            freed += c.w
        return pick, freed >= need

    @staticmethod
    def _trim(pick: list[Cand], need: float) -> list[Cand]:
        """Drop members the plan turned out not to need, most harmful first."""
        keep = list(pick)
        for c in reversed(pick):
            rest = [k for k in keep if k is not c]
            if sum(k.w for k in rest) >= need:
                keep = rest
        return keep

    def standby_plan(self, t: float) -> list[str]:
        """What would open if the sum crossed the shed line right now by the smallest amount."""
        if self.state == "OFF":
            return []
        cands, _ = self.ladder(t)
        need = max(self.total(t), self.cfg.shed_w + 1) - self.cfg.target_w
        pick, ok = self._prefix(cands, need)
        return [c.plug.key for c in (self._trim(pick, need) if ok else pick)]

    # ---- clock ----------------------------------------------------------------------
    def tick(self, t: float) -> None:
        tm = self.tm
        now_w = self.total(t)
        self.samples.append((t, now_w))
        if self.state not in ("BLIND", "STARTING"):  # untrusted sums never enter the hold window
            self.hist.append((t, now_w))
        if len(self.samples) > 4000:
            del self.samples[:2000]
        cut = t - max(tm.max_hold_s, tm.room_window_s) - 5
        if len(self.hist) > 2000 and self.hist[0][0] < cut:
            self.hist = [h for h in self.hist if h[0] >= cut]
        if self.event_open:
            self.peak = max(self.peak, now_w)
        # Fix B: how long the circuit has been over the shed line without a break.
        if now_w > self.cfg.shed_w and self.state in ACTIVE_STATES + ("SHED", "LOCKOUT"):
            if self.over_since is None:
                self.over_since = t
        else:
            self.over_since, self.wait_logged, self.wait_for = None, False, {}
        if self.state == "OFF":
            return
        if self.state in ("HOLD", "RESTORE", "OVER"):  # the calm clock
            if now_w > self.cfg.restore_w:
                self.under_since = None
            elif self.under_since is None:
                self.under_since = t
        for p in self.plugs.values():
            self._plug_tick(p, t)
        if self.state != "BLIND" and self._blind(t):
            self._go_blind(t)
            return
        st = self.state
        if st == "STARTING":
            fresh = all(p.at >= self.started_at or not p.reachable for p in self.plugs.values())
            if fresh or t - self.started_at >= tm.start_timeout_s:
                self.state = "HOLD" if self.event_open else "WATCH"
                self._reset_window(t)
                self._reset_calm(t)
                self._log(t, "armed", state=self.state, sum=round(self.total(t)),
                          waited_s=round(t - self.started_at, 1), all_fresh=fresh)
        elif st == "BLIND":
            ps = list(self.plugs.values())
            all_gone = len(ps) >= 2 and all(not p.reachable for p in ps)
            if all_gone and self.blind_reason != "no_power":
                # Plugs fell silent one after another: now every one is gone.
                self.blind_reason = "no_power"
                self._log(t, "blind", why="every plug on this circuit stopped answering",
                          reason="no_power")
                self._followup(t, "no_power", True)
            elif not all_gone and self.blind_reason == "no_power":
                self.blind_reason = "lost_contact"  # one answers again: not a dead circuit
                self._followup(t, "no_power", False)
                self._log(t, "blind", why="more than half of the plugs are not reachable",
                          reason="lost_contact")
            if not self._blind(t):
                self._log(t, "starting", why="plugs are back")
                if self.blind_reason == "no_power":
                    self._followup(t, "no_power", False)
                self.state, self.started_at, self.blind_reason = "STARTING", t, ""
                self._reset_calm(t)
                for p in self.plugs.values():
                    if p.reachable:
                        p.read_at = t
                        self.eng.act.read(p.key, t)
        elif st == "SHED":
            if self.decide_at is not None and (t >= self.decide_at or self._answered()):
                self._decide_sacrifice(t)
        elif st == "LOCKOUT":
            self._lockout(t)
        elif st in ACTIVE_STATES and not self.eng.switching_allowed:
            # Switching is off in Home Assistant: measure only. One clear entry,
            # no trips, no restores, no tries used up.
            if not self.switching_off_logged:
                self.switching_off_logged = True
                self._log(t, "switching_off", why="switching is turned off in Home Assistant")
            self.hard_since = None
            return
        elif st in ACTIVE_STATES:
            if self.switching_off_logged:
                self.switching_off_logged = False
                self._reset_window(t)
                self._log(t, "switching_on")
            kind = self._tripped(t)
            if kind and self._may_trip(t, kind):
                self._trip(t, kind)
            elif st == "RESTORE":
                if t >= self.restore_until:
                    self.state, self.restoring = "HOLD", None
            elif st in ("HOLD", "OVER"):
                self._hold(t)

    def _plug_tick(self, p: Plug, t: float) -> None:
        tm = self.tm
        # Direct re-read of a fired plug that has not reported low (6.7).
        if p.zeroed and p.confirmed_at is not None and not p.verify_sent \
                and t - p.confirmed_at >= tm.verify_off_s:
            p.verify_sent = True
            p.read_at = t
            self.eng.act.read(p.key, t)
        # A plug that should be off reported power: ask it again before believing it.
        if p.suspect_at is not None and not p.suspect_reread and t - p.suspect_at >= 1.0:
            p.suspect_reread = True
            p.read_at = t
            self.eng.act.read(p.key, t)
        # Heartbeat: the plug has no reporting interval of its own, so ask it.
        since = t - max(p.at, self.started_at)
        if p.available and since >= tm.heartbeat_s and t - p.read_at >= tm.heartbeat_s:
            p.read_at = t
            self.eng.act.read(p.key, t)
        silent = p.available and since > tm.silent_s
        if silent and not p.silent:
            p.silent = True
            self._log(t, "plug_silent", plug=p.key, last_s=round(since, 1))
            if p.relay == "on" and p.role != "other" and self.state != "STARTING":
                p.stuck = True
                self._followup(t, "plug_stuck", True, plug=p.key)
        # Did the heater pick its dry back up? (DESIGN 7)
        if p.dry_check_at is not None and t >= p.dry_check_at:
            p.dry_check_at = None
            if p.relay == "on" and p.w < tm.restart_dry_w and p.dry_was_w >= tm.restart_dry_w:
                self._followup(t, "restart_dry", True, plug=p.key, w=round(p.w, 1))
        # Prediction confirmation: the start's draw fell to half its peak for 30 s.
        f = p.feed
        if p.role == "printer" and not f.confirmed and f.start_t is not None:
            if p.relay != "on" or f.state not in ("starting", "printing"):
                f.confirmed = True
            elif f.start_peak > 100 and p.w <= f.start_peak / 2:
                if f.low_since is None:
                    f.low_since = t
                elif t - f.low_since >= 30:
                    f.confirmed = True
                    real = f.low_since - f.start_t
                    p.starts = (p.starts + [real])[-10:]
                    pred = (f.high_until or t) - f.start_t
                    self._log(t, "confirmation", plug=p.key, predicted_s=round(pred),
                              real_s=round(real), error_s=round(real - pred))
            else:
                f.low_since = None
            if f.state == "starting" or p.w > f.start_peak:
                f.start_peak = max(f.start_peak, p.w)

    def _blind(self, t: float) -> bool:
        ps = list(self.plugs.values())
        return bool(ps) and sum(1 for p in ps if not p.reachable) * 2 > len(ps)

    def _go_blind(self, t: float) -> None:
        ps = list(self.plugs.values())
        all_gone = all(not p.reachable for p in ps)
        self.blind_reason = "no_power" if all_gone and len(ps) >= 2 else "lost_contact"
        self.state = "BLIND"
        self.hard_since = None
        self._reset_calm(t)
        why = ("every plug on this circuit stopped answering" if self.blind_reason == "no_power"
               else "more than half of the plugs are not reachable")
        self._log(t, "blind", why=why, reason=self.blind_reason)
        if self.blind_reason == "no_power":
            self._followup(t, "no_power", True)

    def _reset_calm(self, t: float) -> None:
        """The restore calm clock and its hold window start again (BLIND, STARTING)."""
        self.under_since = None
        self.hist = []

    def _reset_window(self, t: float) -> None:
        self.window_from = t
        self.samples = [(t, self.total(t))]

    def _tripped(self, t: float) -> str | None:
        """"hard", "shed" or None."""
        if self.total(t) >= self.cfg.hard_w:  # hard line: two looks 250 ms apart, no window
            if self.hard_since is None:
                self.hard_since = t
            if t - self.hard_since >= self.tm.hard_confirm_s:
                return "hard"
        else:
            self.hard_since = None
        m = self.mean(t)
        return "shed" if m is not None and m > self.cfg.shed_w else None

    def over_s(self, t: float) -> float:
        return 0.0 if self.over_since is None else t - self.over_since

    def _printer_trigger(self, kind: str, t: float) -> bool:
        """Fix B: a printer may be cut over the hard line, or after printer_sustain_s
        over the shed line without a break."""
        return kind == "hard" or self.over_s(t) >= self.tm.printer_sustain_s

    def _may_trip(self, t: float, kind: str) -> bool:
        """Heaters keep the fast trigger. When only printers are left to cut, the
        trip waits for the printer trigger (fix B) and says so once."""
        can_sacrifice = self.state != "OVER" and self.rounds < self.tm.max_rounds
        cands, _ = self.ladder(t, 3 if can_sacrifice else 2)
        printers = [c for c in cands if c.tier >= 2]
        if any(c.tier == 1 for c in cands) or not printers:
            return True
        if self.wait_for and self.total(t, verified_only=True) <= self.cfg.shed_w and all(
                (p.at, p.reachable) == self.wait_for[k]
                for k, p in self.plugs.items() if k in self.wait_for):
            # Nothing new from the plugs it could not check, and the plugs it can check
            # are not over the line by themselves: no new look yet.
            return False
        if self._printer_trigger(kind, t):
            return True
        self._wait_log(t, printers)
        return False

    def _wait_log(self, t: float, printers: list[Cand]) -> None:
        # Only while the sum itself is over the line (not just the tail of the 2 s mean).
        if not self.wait_logged and self.over_since is not None \
                and t - self.wait_log_at >= self.tm.printer_wait_log_s:
            self.wait_logged, self.wait_log_at = True, t
            self._log(t, "printer_wait", sum=round(self.total(t)), over_s=round(self.over_s(t), 1),
                      wait_s=self.tm.printer_sustain_s, plugs=[c.plug.key for c in printers])

    def _answered(self) -> bool:
        """Fix A: every plug asked for a fresh reading has answered."""
        at = self.refresh_at if self.refresh_at is not None else 0.0
        return all(self.plugs[k].at >= at for k in self.refresh_keys
                   if k in self.plugs and self.plugs[k].relay == "on" and not self.plugs[k].zeroed)

    # ---- one shot (DESIGN 6.4 to 6.10) --------------------------------------------------
    def _trip(self, t: float, kind: str = "shed") -> None:
        cfg, tm = self.cfg, self.tm
        s_now, m = self.total(t), self.mean(t)
        judged = max(s_now, m or 0.0)
        can_sacrifice = self.state != "OVER" and self.rounds < tm.max_rounds  # rule 6
        cands, skipped = self.ladder(t, 3 if can_sacrifice else 2)
        need = judged - cfg.target_w
        heaters = [c for c in cands if c.tier == 1]
        printers = [c for c in cands if c.tier >= 2]
        if not heaters and not printers:
            if not self.over_logged:
                self.over_logged = True
                self._open_event(t)
                self._log(t, "cant_protect", sum=round(judged),
                          why="nothing left that may be turned off", skipped=skipped)
                self._followup(t, "cant_protect", True, sum=round(judged))
            self.state = "OVER"
            return
        self._open_event(t)
        self.rounds += 1
        self.over_logged = False
        self.hard_since = None
        self._log(t, "trip", round=self.rounds, sum=round(judged), sum_now=round(s_now),
                  shed_w=cfg.shed_w, target=cfg.target_w, need=round(need), line=kind,
                  over_s=round(self.over_s(t), 1),
                  ladder=[[c.plug.key, c.tier, round(c.w), c.value, c.note] for c in cands],
                  skipped=[list(s) for s in skipped])
        pick, ok = self._prefix(heaters, need)
        self.fired, self.outcome, self.decide_at = [], "ok", None
        if ok:  # the common case: heaters only, on the fast trigger
            self._fire(t, self._trim(pick, need))
            self._start_lockout(t)
            return
        self._fire(t, heaters)  # every heater is in any larger plan
        if not printers:
            self.outcome = "cant_reach_target"
            self._start_lockout(t)
            return
        if not self._printer_trigger(kind, t):
            # Fix B: the heaters went at once; a printer waits for the slow trigger.
            self.outcome = "printer_wait"
            self._wait_log(t, printers)
            self._start_lockout(t)
            return
        self.shot_from = self.state
        self._refresh(t, 3 if can_sacrifice else 2)

    def _refresh(self, t: float, max_tier: int) -> None:
        """Fix A: before any printer is cut, read every plug on the circuit again."""
        self.shot_tier, self.refresh_at = max_tier, t
        keys = []
        for p in self.plugs.values():
            if p.relay == "on" and not p.zeroed and p.reachable:
                p.read_at = t
                if self.eng.act.read(p.key, t):
                    keys.append(p.key)
        self.refresh_keys = keys
        self.decide_at = t + self.tm.refresh_timeout_s
        self.state = "SHED"
        self._log(t, "refresh", plugs=keys, all=True)

    def _decide_sacrifice(self, t: float) -> None:
        cfg = self.cfg
        self.decide_at = None
        s_ver = self.total(t, verified_only=True)  # rule 5 and fix A: fresh readings only
        unverified = [p.key for p in self.plugs.values()
                      if not self._verified(p, t) and self.eff(p, t) > 0]
        cands = [c for c in self.ladder(t, self.shot_tier)[0]
                 if c.tier >= 2 and self._verified(c.plug, t)]
        need = s_ver - cfg.target_w
        hard = s_ver >= cfg.hard_w
        pick: list[Cand] = []
        if s_ver <= cfg.shed_w:  # fix A: under the shed line on fresh numbers: no printer is cut
            if self.total(t) <= cfg.shed_w:
                self.outcome = "ok"
            elif unverified and all(self.plugs[k].reachable for k in unverified):
                # A plug that answers only missed its re-read: look again once it has
                # something new, instead of giving up on the circuit (OVER). A plug that
                # stays quiet is marked not reachable after silent_s, then this flags.
                self.outcome = "recheck"
            else:
                self.outcome = "unverified_over"
        elif not self._printer_trigger("hard" if hard else "shed", t):
            self.outcome = "printer_wait"  # fix B: over the shed line, not yet for 60 s
        else:
            pick, ok = self._prefix(cands, need)
            if ok:
                pick = self._trim(pick, need)
            else:  # the target cannot be reached: settle for the hard line
                need_h = s_ver - cfg.hard_w
                pick, ok_h = self._prefix(cands, need_h) if need_h > 0 else ([], True)
                if need_h > 0 and not ok_h:
                    pick, self.outcome = [], "cant_protect"
                else:
                    pick, self.outcome = self._trim(pick, need_h), "cant_reach_target"
        self._log(t, "sacrifice_decision", verified_sum=round(s_ver), need=round(need),
                  unverified=unverified, plugs=[c.plug.key for c in pick], outcome=self.outcome,
                  values=[[c.plug.key, c.value] for c in cands], hard=hard,
                  over_s=round(self.over_s(t), 1),
                  answered=self._answered(), waited_s=round(t - (self.refresh_at or t), 2))
        if self.outcome == "cant_protect":
            self._followup(t, "cant_protect", True, sum=round(self.total(t)))
        self.wait_for = ({k: (self.plugs[k].at, self.plugs[k].reachable) for k in unverified}
                         if self.outcome in ("recheck", "unverified_over") else {})
        self._fire(t, pick)
        if not self.fired:
            self.rounds -= 1  # rule 6 counts rounds that turned something off
            if self.outcome == "recheck":
                # Nothing is settling, so no lockout: the next look comes as soon as a
                # plug it could not check has something new (wait_for), while that is
                # still fresh. A lockout here can phase-lock with a slow reporter.
                self.state = "OVER" if self.shot_from == "OVER" else "HOLD"
                return
        self._start_lockout(t)

    def _fire(self, t: float, pick: list[Cand]) -> None:
        for c in pick:
            p = c.plug
            p.shed_w = c.w
            p.zeroed, p.fired_at, p.confirmed_at = True, t, None  # rule 2: zero first, then send
            p.retried = p.verify_sent = False
            p.shed_kind = "heater" if c.tier == 1 else "idle" if c.tier == 2 else "sacrifice"
            p.shed_at, p.blocked_logged, p.booked_until = t, False, -1e9
            p.shed_value, p.shed_job = p.feed.value, p.feed.job
            p.dry_check_at = None
            if t - p.restored_at < self.tm.reshed_window_s:
                p.hold_mult = min(p.hold_mult * 2, 64)
            self.fired.append(p)
        for c in pick:  # all off commands go out together (6.5)
            self.eng._send(t, c.plug, "off")
        if pick:
            self._log(t, "shed", plugs=[[c.plug.key, round(c.w), c.tier] for c in pick],
                      freed=round(sum(c.w for c in pick)), sum_after=round(self.total(t)))
        for c in pick:
            p = c.plug
            if p.shed_kind == "sacrifice":
                started = p.feed.state == "starting"
                self.lost_print = self.lost_print or not started
                self._followup(t, "sacrificed", True, plug=p.key, label=p.label,
                               starting=started, value_mills=p.shed_value,
                               basis=p.feed.basis, job=p.shed_job)
            elif p.shed_kind == "idle":
                self._followup(t, "idle_off", True, plug=p.key, label=p.label)

    def _start_lockout(self, t: float) -> None:
        self.state, self.lock_at = "LOCKOUT", t
        self.under_since = None

    def _lockout(self, t: float) -> None:  # rule 3: nothing is evaluated here
        tm, el = self.tm, t - self.lock_at
        settled = all((not p.zeroed) or p.failed_open for p in self.fired)
        if (settled and el >= tm.lockout_min_s) or el >= tm.lockout_max_s:
            self._reset_window(t)
            s = self.total(t)
            bad = self.outcome in ("cant_protect", "cant_reach_target", "unverified_over") \
                and s > self.cfg.shed_w
            self.state = "OVER" if bad else "HOLD"
            self._log(t, "lockout_end", after_s=round(el, 2), sum=round(s), state=self.state,
                      outcome=self.outcome, still_zeroed=[p.key for p in self.fired if p.zeroed])
            if bad and self.outcome != "cant_protect":
                self._followup(t, "cant_protect", True, sum=round(s), outcome=self.outcome)
            if self.pending is not None and (not self.pending.protect or self.drop_after):
                self.eng._apply_pending(self, t)  # protection off waits for the lockout, no longer

    # ---- restore (DESIGN 7, 4C) --------------------------------------------------------
    def waiting(self) -> list[Plug]:
        return sorted(
            (p for p in self.held() if not p.gave_up),
            key=lambda p: (p.shed_kind != "heater", p.shed_at, p.key))

    def held(self) -> list[Plug]:
        """Heaters and idle printers the engine turned off and still owns (gave up included)."""
        return [p for p in self.plugs.values()
                if p.shed_kind in ("heater", "idle") and p.relay == "off"
                and not p.failed_open and p.controllable]

    def predicted_floor(self) -> tuple[float | None, Plug | None]:
        best: tuple[float | None, Plug | None] = (None, None)
        for p in self.plugs.values():
            f = p.feed
            if p.role == "printer" and p.relay == "on" and f.high_until is not None \
                    and f.state in ("starting", "printing"):
                if best[0] is None or f.high_until > best[0]:
                    best = (f.high_until, p)
        return best

    def worst(self, t: float, window: float) -> float:
        return max((v for ts, v in self.hist if ts >= t - window), default=self.total(t))

    def _hold(self, t: float) -> None:
        cfg, tm, s = self.cfg, self.tm, self.total(t)
        if self.state == "OVER":
            if self.under_since is not None and t - self.under_since >= tm.restore_hold_s:
                self.state = "HOLD"
                self._log(t, "over_cleared", sum=round(s))
                self._followup(t, "cant_protect", False)
            return
        waiting = self.waiting()
        if not waiting:
            # The event stays open while a heater that used its tries is still off
            # (a person can say Try again). Nothing held: back to watching.
            if s <= cfg.shed_w and not self.held():
                if self.event_open:
                    self._close_event(t)
                else:
                    self.state = "WATCH"
            return
        if self.under_since is None:
            return
        floor, _ = self.predicted_floor()
        if floor is not None and t < floor:
            return  # the predicted heat-up window is not over yet
        heater_pending = False
        for p in waiting:
            window = min(tm.restore_hold_s * p.hold_mult, tm.max_hold_s)
            if p.shed_kind == "idle" and heater_pending:
                return  # strictly heaters first: an idle printer waits for them
            if t - self.under_since < window:
                if p.shed_kind == "heater":
                    heater_pending = True  # it may still fit when its window is over
                continue
            hold_max = self.worst(t, window)  # worst sum in the hold window
            if p.tries >= tm.restore_tries:
                p.gave_up = True
                self._log(t, "restore_gave_up", plug=p.key, tries=p.tries)
                self._followup(t, "restore_blocked", True, plug=p.key, tries=p.tries)
                continue
            need = p.expected_w(tm)
            if hold_max + need > cfg.target_w:
                if not p.blocked_logged:
                    p.blocked_logged = True
                    self._log(t, "restore_blocked", plug=p.key, hold_max=round(hold_max),
                              needs=round(need), room=round(cfg.target_w - hold_max))
                continue
            p.tries += 1
            self._turn_on(t, p, need, hold_max, s, "restore")
            return

    def _turn_on(self, t: float, p: Plug, need: float, hold_max: float, s: float, kind: str,
                 **extra: Any) -> None:
        p.restored_at = t
        p.booked_w, p.booked_until = need, t + self.tm.restore_settle_s  # booked at once
        self.state, self.restore_until, self.restoring = "RESTORE", t + self.tm.restore_settle_s, p.key
        self._log(t, kind, plug=p.key, reserved=round(need), hold_max=round(hold_max),
                  held_s=round(t - (self.under_since or t)), sum_before=round(s), **extra)
        self.eng._send(t, p, "on")

    # ---- people (CONTRACT 0.5) ---------------------------------------------------------
    def power_on(self, t: float, p: Plug) -> tuple[bool, str]:
        if p.role != "printer":
            return False, "Only a printer's power can be turned back on here. Heaters come back by themselves."
        if p.relay == "on" and p.pending != "off":
            return False, "It already has power."
        if p.shed_kind not in ("idle", "sacrifice"):
            return False, "This plug was not turned off by power protection, so it stays as it is."
        if self.state in ("SHED", "LOCKOUT"):
            return False, "Power protection is turning things off right now. Try again in a minute."
        if self.state in ("BLIND", "STARTING", "OFF"):
            return False, "Power protection cannot see this circuit right now. Use the button on the plug if you are sure."
        if self.state == "OVER":
            return False, "The power on this circuit is still too high."
        if self.state == "RESTORE":
            return False, "Something else is being turned back on. Try again in a minute."
        need = p.expected_w(self.tm)
        if not self.eng.switching_allowed:
            return False, "Switching is turned off in Home Assistant. Use the button on the plug."
        worst = self.worst(t, self.tm.room_window_s)
        if worst + need > self.cfg.target_w:
            return False, (f"There is not enough room on {self.cfg.name} right now "
                           f"({round(worst)} W in use, it needs about {round(need)} W). "
                           "It can come back when the power drops.")
        p.person_on = True
        self._turn_on(t, p, need, worst, self.total(t), "restore", by_person=True)
        return True, "Turning its power back on."

    def retry(self, t: float, p: Plug) -> tuple[bool, str]:
        if p.role not in HEATERS:
            return False, "Try again is for heaters and dryers."
        if p.shed_kind != "heater":
            return False, "This plug is not being held off."
        p.tries, p.gave_up, p.hold_mult, p.blocked_logged = 0, False, 1, False
        if self.state == "WATCH":
            self.state = "HOLD"  # so the restore rules run again
        self._followup(t, "restore_blocked", False, plug=p.key)
        return True, "It comes back by itself when there is room."

    # ---- helpers --------------------------------------------------------------------
    def _open_event(self, t: float) -> None:
        if not self.event_open:
            self.event += 1
            self.event_open, self.rounds = True, 0
            self.opened_at, self.peak, self.lost_print = t, self.total(t), False
            for p in self.plugs.values():
                p.tries, p.gave_up = 0, False
            self._log(t, "event_open", sum=round(self.total(t)))

    def _close_event(self, t: float) -> None:
        s = self.total(t)
        self._log(t, "all_clear", sum=round(s), rounds=self.rounds)
        self._log(t, "event_close", minutes=round((t - self.opened_at) / 60, 1),
                  rounds=self.rounds, peak=round(self.peak), lost_print=self.lost_print)
        self.event_open = False
        self.closed_at = t
        self.state = "WATCH"
        self._followup(t, "cant_protect", False)
        if self.pending is not None:
            self.eng._apply_pending(self, t)

    def _log(self, t: float, entry: str, plug: str | None = None, **data: Any) -> None:
        self.eng.journal.add(t, entry, self.cfg.id, plug, self.event or None,
                             {"state": self.state, **data})
        self.eng.trace.append((t, entry, {"state": self.state, "plug": plug, **data}))

    def _followup(self, t: float, kind: str, open_: bool, plug: str | None = None, **data: Any) -> None:
        key = (kind, plug)
        was = key in self.eng.open_followups.get(self.cfg.id, set())
        if open_ == was and kind not in ("sacrificed", "idle_off", "restart_dry"):
            return
        opened = self.eng.open_followups.setdefault(self.cfg.id, set())
        if open_:
            opened.add(key)
        else:
            if not was:
                return
            opened.discard(key)
        self._log(t, "followup", plug=plug, kind=kind, open=open_, **data)


class Engine:
    """Every circuit at the site, the plugs, the feed and the journal."""

    def __init__(self, actuator: Actuator, *, clock: Callable[[], float] | None = None,
                 wall: Callable[[], str] | None = None, engine_id: str = "engine",
                 timers: Timers | None = None) -> None:
        self.act = actuator
        self.tm = timers or Timers()
        self.now = clock or (lambda: self._last_t)
        self.engine_id = engine_id
        self.journal = Journal(wall or (lambda: ""))
        self.trace: list[tuple[float, str, dict[str, Any]]] = []
        self.commands: list[tuple[float, str, str]] = []
        self.plugs: dict[str, Plug] = {}
        self.circuits: dict[str, Circuit] = {}
        self.config: dict[str, Any] = {}
        self.config_version = 0
        self.config_errors: list[str] = []
        self.switching_allowed = True
        self.open_followups: dict[str, set] = {}
        self.feed_at = -1e9
        self._last_t = 0.0
        self.faults = 0
        self._deferred: set[str] = set()

    def _entry(self, t: float, kind: str, circuit: str | None, plug: str | None,
               event: int | None, data: dict[str, Any]) -> None:
        self.journal.add(t, kind, circuit, plug, event, data)
        self.trace.append((t, kind, {"plug": plug, "circuit": circuit, **data}))
        if len(self.trace) > 20000:
            del self.trace[:10000]

    # ---- config (CONTRACT 1.3) ---------------------------------------------------------
    def apply_config(self, doc: dict[str, Any], t: float) -> dict[str, Any]:
        """Apply a config document. A circuit with an open event keeps its old
        settings until the event ends, except turning protection off."""
        errors: list[str] = []
        new: dict[str, CircuitCfg] = {}
        for d in doc.get("circuits") or []:
            c, errs = validate_circuit(d)
            errors += errs
            if c is not None:
                new[c.id] = c
        plug_docs: dict[str, dict[str, Any]] = {}
        for d in doc.get("plugs") or []:
            key = str(d.get("ieee") or "").strip().lower()
            if not key:
                errors.append("a plug without an address")
                continue
            role = d.get("role") if d.get("role") in ROLES else "other"
            plug_docs[key] = {**d, "role": role}
        self.config = {"version": int(doc.get("version") or 0), "circuits": list(new),
                       "doc": doc}
        self.config_version = self.config["version"]
        self.config_errors = errors
        self._plug_docs = plug_docs
        self._circuit_cfgs = new
        pending = []
        for cid, c in list(self.circuits.items()):
            if cid not in new:
                if c.state in ("SHED", "LOCKOUT"):
                    pending.append(cid)
                    c.pending = CircuitCfg(id=cid, name=c.cfg.name, protect=False,
                                           breaker_amps=c.cfg.breaker_amps, volts=c.cfg.volts,
                                           shed_w=c.cfg.shed_w, restore_w=c.cfg.restore_w,
                                           hard_w=c.cfg.hard_w)
                    c.drop_after = True
                    continue
                self._drop_circuit(c, t)
        for cid, cfg in new.items():
            c = self.circuits.get(cid)
            if c is None:
                c = Circuit(self, cfg)
                c.started_at = t
                self.circuits[cid] = c
                self._bind(c, t)
                if c.state == "STARTING":
                    self._read_all(c, t)
                self._log_config(c, t)
                continue
            in_shot = c.state in ("SHED", "LOCKOUT")
            busy = c.event_open or in_shot
            c.pending = cfg
            if (busy and cfg.protect) or in_shot:
                # Waits for the event to end; protection off waits only for the lockout.
                pending.append(cid)
            else:
                self._apply_pending(c, t)
        # Plugs on no circuit (or a circuit that is not there) are only measured.
        for key, d in plug_docs.items():
            held = self.plugs.get(key)
            if held is not None and held.circuit in self.circuits \
                    and self.circuits[held.circuit].pending is not None:
                continue  # its circuit is busy: it moves when the event ends
            if d.get("circuit") not in self.circuits:
                p = self._plug(key)
                self._set_plug(p, d)
                p.circuit = None
        for key, p in self.plugs.items():
            if key not in plug_docs and p.circuit and p.circuit in self.circuits \
                    and self.circuits[p.circuit].pending is None:
                self.circuits[p.circuit].plugs.pop(key, None)
                p.circuit = None
        self._entry(t, "config_applied", None, None, None,
                         {"version": self.config_version, "errors": errors, "waiting": pending})
        return {"accepted": not errors or bool(new), "version": self.config_version,
                "errors": errors, "waiting": pending}

    def _drop_circuit(self, c: Circuit, t: float) -> None:
        for p in c.plugs.values():
            p.circuit = None
        self.circuits.pop(c.cfg.id, None)
        self._entry(t, "config_applied", c.cfg.id, None, None, {"removed": True})

    def _apply_pending(self, c: Circuit, t: float) -> None:
        cfg = c.pending
        if cfg is None:
            return
        if c.state in ("SHED", "LOCKOUT"):
            return  # never mid-shot: applied at the end of the lockout
        c.pending = None
        if c.drop_after:
            self._drop_circuit(c, t)
            self._rebind_deferred(t)
            return
        was_off = c.state == "OFF"
        c.cfg = cfg
        if not cfg.protect:
            # Protection off: nothing more is switched. Plugs held off stay off.
            if c.event_open:
                c._log(t, "event_close", minutes=round((t - c.opened_at) / 60, 1),
                       rounds=c.rounds, peak=round(c.peak), lost_print=c.lost_print,
                       why="protection turned off")
                c.event_open = False
                c.closed_at = t
            c.state = "OFF"
        self._bind(c, t)
        if cfg.protect and (was_off or c.state == "OFF") and c.plugs:
            c.state, c.started_at = "STARTING", t
            self._read_all(c, t)
        self._log_config(c, t)
        self._rebind_deferred(t)

    @staticmethod
    def _busy(c: Circuit) -> bool:
        return c.event_open or c.pending is not None or c.state in ("SHED", "LOCKOUT")

    def _rebind_deferred(self, t: float) -> None:
        """Plugs that move between circuits wait until both circuits are quiet (row 16)."""
        for cid in sorted(self._deferred):
            c = self.circuits.get(cid)
            if c is None:
                self._deferred.discard(cid)
            elif not self._busy(c):
                self._deferred.discard(cid)
                self._bind(c, t)

    def _bind(self, c: Circuit, t: float) -> None:
        docs = getattr(self, "_plug_docs", {})
        keep = {k for k, d in docs.items() if d.get("circuit") == c.cfg.id}
        for key in list(c.plugs):
            if key not in keep:
                c.plugs.pop(key).circuit = None
        added = False
        for key in keep:
            p = self._plug(key)
            if key not in c.plugs:
                prev = self.circuits.get(p.circuit) if p.circuit else None
                if prev is not None and prev is not c:
                    if self._busy(prev):
                        self._deferred.add(c.cfg.id)  # moves when its old circuit is quiet
                        continue
                    prev.plugs.pop(key, None)
                added = True
            self._set_plug(p, docs[key])
            p.circuit = c.cfg.id
            c.plugs[key] = p
        if not c.plugs or not c.cfg.protect:
            if c.state not in ("SHED", "LOCKOUT"):
                c.state = "OFF"
        elif c.state == "OFF" or (added and c.state == "WATCH"):
            c.state, c.started_at = "STARTING", t
            self._read_all(c, t)

    def _read_all(self, c: Circuit, t: float) -> None:
        for p in c.plugs.values():
            p.read_at = t
            self.act.read(p.key, t)

    def _log_config(self, c: Circuit, t: float) -> None:
        c._log(t, "circuit_config", protect=c.cfg.protect, plugs=sorted(c.plugs),
               shed_w=c.cfg.shed_w, restore_w=c.cfg.restore_w, hard_w=c.cfg.hard_w,
               unmetered_w=c.cfg.unmetered_w)

    @staticmethod
    def _set_plug(p: Plug, d: dict[str, Any]) -> None:
        p.role = d["role"]
        p.label = str(d.get("label") or p.label or p.key)[:80]
        p.eligible = bool(d.get("may_turn_off", True)) and p.role != "other"
        rated = d.get("rated_w")
        p.peak_w = float(rated) if isinstance(rated, (int, float)) and rated > 0 else p.peak_w
        serial = d.get("printer_serial")
        p.printer_serial = str(serial).upper() if serial else None

    def _plug(self, key: str) -> Plug:
        p = self.plugs.get(key)
        if p is None:
            p = self.plugs[key] = Plug(key)
        return p

    # ---- test and glue helpers ---------------------------------------------------------
    def add_circuit(self, cfg: CircuitCfg, plugs: list[Plug], t: float = 0.0) -> Circuit:
        """Direct setup without a document (tests)."""
        c = Circuit(self, cfg)
        c.started_at = t
        for p in plugs:
            p.circuit = cfg.id
            if p.role == "other":
                p.eligible = False
            self.plugs[p.key] = p
            c.plugs[p.key] = p
        if not plugs or not cfg.protect:
            c.state = "OFF"
        self.circuits[cfg.id] = c
        return c

    def circuit_of(self, key: str) -> Circuit | None:
        p = self.plugs.get(key)
        return self.circuits.get(p.circuit) if p and p.circuit else None

    # ---- inputs ------------------------------------------------------------------------
    def on_power(self, key: str, w: float, t: float, measured: float | None = None) -> None:
        self._guarded(key, t, self._on_power, key, w, t, measured)

    def _off_but_drawing(self, p: Plug, w: float, t: float, m: float, c: Circuit | None,
                         why: str) -> None:
        """A plug that should be off reported power. Believed only when it was measured
        well after the relay confirmed off and a direct re-read agrees (rule 4)."""
        since = m - (p.confirmed_at if p.confirmed_at is not None else (p.fired_at or m))
        if since < self.tm.verify_off_s:
            return  # measured before the cut, still in flight
        if p.suspect_at is None or t - p.suspect_at > 10:
            p.suspect_at, p.suspect_reread, p.read_at = t, False, t
            self.act.read(p.key, t)
            return
        if m < p.suspect_at + 1.0:
            return  # not the re-read yet: could be another late report
        p.suspect_at = None
        p.zeroed, p.failed_open, p.w, p.at = False, True, w, m
        if c:
            c._log(t, "failed_open", plug=p.key, w=w, why=why)
            c._followup(t, "failed_open", True, plug=p.key, label=p.label, w=round(w))

    def _on_power(self, key: str, w: float, t: float, measured: float | None) -> None:
        p = self._plug(key)
        tm = self.tm
        if w is None or not math.isfinite(w) or w < 0:
            return
        m = t if measured is None else min(measured, t)
        if m < p.at:
            return  # older than what we already have
        if w > tm.max_plug_w:  # the plug cannot carry it: discard and re-read
            p.read_at = t
            self.act.read(key, t)
            return
        c = self.circuit_of(key)
        if p.at > -1e8:
            p.gaps = (p.gaps + [round(t - p.at, 2)])[-30:]
        if not p.available and m > p.unavailable_at:
            p.available = True  # a valid reading taken after it went away: it answers again
            if c:
                c._log(t, "plug_back", plug=key, via="reading")
        if p.silent:
            p.silent = False
            if c:
                c._log(t, "plug_back", plug=key)
                if p.stuck:
                    p.stuck = False
                    c._followup(t, "plug_stuck", False, plug=key)
        if p.zeroed:
            if p.confirmed_at is None:
                return  # rule 2: measured before the relay opened
            if w <= tm.off_w:
                p.zeroed, p.w, p.at = False, w, m  # a fresh post-off reading replaces the zero
                p.suspect_at = None
                if c:
                    c._log(t, "shed_settled", plug=key, w=w)
            else:
                self._off_but_drawing(p, w, t, m, c, "still drawing after it confirmed off")
            return
        if p.shed_kind and p.relay == "off" and not p.failed_open:
            if w > tm.off_w:
                self._off_but_drawing(p, w, t, m, c, "drawing power while held off")
                return
            p.suspect_at = None
        p.w, p.at = w, m
        if p.failed_open and w <= tm.off_w and p.relay == "off" and c:
            p.failed_open = False
            c._followup(t, "failed_open", False, plug=key)
        if p.relay == "on" and p.role in HEATERS and w >= tm.restart_dry_w:
            p.drew_at = m  # fix C: still in its heating session
        if p.relay == "on" and w >= tm.min_useful_w:
            if p.role == "printer" and p.feed.state in NOT_PRINTING:
                p.idle_hist = (p.idle_hist + [w])[-200:]
            elif p.role != "printer":
                p.hist = (p.hist + [w])[-500:]
            if c and p.dry_check_at is None and p.role in HEATERS and w >= tm.restart_dry_w \
                    and ("restart_dry", key) in self.open_followups.get(c.cfg.id, set()):
                c._followup(t, "restart_dry", False, plug=key)

    def on_command(self, key: str, action: str, ok: bool, t: float) -> None:
        self._guarded(key, t, self._on_command, key, action, ok, t)

    def _on_command(self, key: str, action: str, ok: bool, t: float) -> None:
        p = self._plug(key)
        c = self.circuit_of(key)
        if p.pending == action:
            p.pending = None
        if action == "off":
            if p.relay == "off" and p.confirmed_at is not None and p.fired_at is not None \
                    and p.confirmed_at >= p.fired_at:
                return  # already confirmed (by the relay's own report or the call)
            if ok:
                p.relay, p.confirmed_at = "off", t
                if c:
                    c._log(t, "shed_confirmed", plug=key,
                           ms=round((t - (p.fired_at or t)) * 1000))
            elif not p.retried:
                p.retried = True
                self._send(t, p, "off")
            else:
                # Rule 4: evidence. It counts again, but the engine remembers it meant to
                # shed it: if the relay reports off later, that is a late confirm.
                p.zeroed, p.failed_open = False, True
                if c:
                    c._log(t, "failed_open", plug=key, w=p.w, why="off command failed twice")
                    c._followup(t, "failed_open", True, plug=key, label=p.label, w=round(p.w))
        else:
            if ok:
                if p.relay == "on" and p.shed_kind is None:
                    return
                kind = p.shed_kind
                p.relay, p.shed_kind = "on", None
                if c:
                    c._log(t, "restore_confirmed", plug=key, by_person=p.person_on)
                    if kind == "idle":
                        c._followup(t, "idle_off", False, plug=key)
                    elif kind == "sacrifice":
                        c._followup(t, "sacrificed", False, plug=key, power_back=True)
                    elif kind == "heater":
                        c._followup(t, "restore_blocked", False, plug=key)
                        p.dry_was_w = max(p.shed_w, p.learned_w)
                        p.dry_check_at = t + self.tm.restart_dry_s
                p.person_on = False
            else:
                p.booked_until = t
                p.person_on = False
                if c:
                    c._log(t, "restore_failed", plug=key)

    def on_relay(self, key: str, state: str, t: float) -> None:
        """The plug's switch entity changed (on, off, unavailable)."""
        self._guarded(key, t, self._on_relay, key, state, t)

    def _on_relay(self, key: str, state: str, t: float) -> None:
        p = self._plug(key)
        if state not in ("on", "off"):
            self.on_reachable(key, False, t)
            return
        if not p.available:
            self.on_reachable(key, True, t)
        if p.pending == state and t - p.pending_at <= 10:
            self.on_command(key, state, True, t)
            return
        if p.zeroed and state == "off":
            return
        c = self.circuit_of(key)
        if state == "off" and p.relay == "on" and p.shed_kind and p.failed_open:
            # The off command timed out but the relay did open: a late confirm.
            p.relay, p.confirmed_at, p.failed_open = "off", t, False
            p.zeroed, p.verify_sent, p.suspect_at = True, False, None
            if c:
                c._log(t, "shed_confirmed", plug=key, late=True)
                c._followup(t, "failed_open", False, plug=key)
            return
        if state == p.relay:
            return
        was = p.shed_kind
        p.relay = state
        if c:
            c._log(t, "external_change", plug=key, relay=state, held_off=was or "")
        if state == "on":
            # A person turned it on: it is not the engine's to restore any more.
            p.zeroed, p.shed_kind, p.booked_until = False, None, -1e9
            p.failed_open = False
            if c and was == "sacrifice":
                c._followup(t, "sacrificed", False, plug=key, power_back=True)
            elif c and was == "idle":
                c._followup(t, "idle_off", False, plug=key)
            elif c and was == "heater":
                c._followup(t, "restore_blocked", False, plug=key)
        else:
            p.dry_check_at = None

    def on_reachable(self, key: str, ok: bool, t: float) -> None:
        p = self._plug(key)
        if not ok and p.available:
            p.unavailable_at = t
        p.available = ok
        if ok:
            p.silent = False

    def feed_plug(self, key: str, state: str, value: int | None, t: float,
                  high_until: float | None = None, job: str = "", basis: str = "") -> None:
        p = self._plug(key)
        f = p.feed
        state = state if state in PRINTER_STATES else "unknown"
        if state == "starting" and f.state != "starting":
            f.start_t, f.start_peak, f.low_since, f.confirmed = t, p.w, None, False
            if high_until is None:
                learned = len(p.starts) >= 3
                secs = p_n(p.starts, 0.8) if learned else self.tm.predict_seed_s
                secs = min(secs, self.tm.predict_cap_s)
                high_until = t + secs
                c = self.circuit_of(key)
                if c:
                    c._log(t, "prediction", plug=key, predicted_s=round(secs),
                           basis="learned" if learned else "seed")
        if state not in ("starting", "printing", "paused"):
            f.high_until = None
        f.state, f.value, f.at = state, (0 if state == "starting" else value), t
        f.job, f.basis = job or f.job, basis or ("start" if state == "starting" else f.basis)
        if high_until is not None:
            f.high_until = high_until

    def on_feed(self, frame: dict[str, Any], t: float) -> None:
        """A feed frame from the link: {"printers": [{serial, state, value_mills, ...}]}."""
        self._guarded(None, t, self._on_feed, frame, t)

    def _guarded(self, key: str | None, t: float, fn: Callable[..., Any], *args: Any) -> Any:
        """An exception in an input handler stops that circuit (failure mode 23)."""
        try:
            return fn(*args)
        except Exception as err:  # noqa: BLE001
            self.faults += 1
            c = self.circuit_of(key) if key else None
            if c is not None:
                c.state = "OFF"
                c._log(t, "fault", plug=key, error=type(err).__name__, detail=str(err)[:200])
            else:
                self._entry(t, "fault", None, key, None,
                            {"error": type(err).__name__, "detail": str(err)[:200]})
            return None

    def _on_feed(self, frame: dict[str, Any], t: float) -> None:
        self.feed_at = t
        by_serial: dict[str, list[Plug]] = {}
        for p in self.plugs.values():
            if p.role == "printer" and p.printer_serial:
                by_serial.setdefault(p.printer_serial, []).append(p)
        for pr in frame.get("printers") or []:
            serial = str(pr.get("serial") or "").upper()
            age = max(0.0, float(pr.get("age_ms") or 0) / 1000.0)
            value = pr.get("value_mills")
            value = int(value) if isinstance(value, (int, float)) else None
            for p in by_serial.get(serial, []):
                self.feed_plug(p.key, str(pr.get("state") or "unknown"), value, t - age,
                               job=str(pr.get("job") or "")[:80], basis=str(pr.get("basis") or ""))

    # ---- clock ----------------------------------------------------------------------
    def tick(self, t: float) -> None:
        self._last_t = t
        for c in list(self.circuits.values()):
            try:
                c.tick(t)
            except Exception as err:  # noqa: BLE001 (failure mode 23: stop, never loop)
                self.faults += 1
                c.state = "OFF"
                c._log(t, "fault", error=type(err).__name__, detail=str(err)[:200])

    def _send(self, t: float, p: Plug, action: str) -> None:
        if not self.switching_allowed or not p.controllable:
            c = self.circuit_of(p.key)
            if c:
                c._log(t, "switch_blocked", plug=p.key, action=action,
                       why="switching is turned off in Home Assistant" if not self.switching_allowed
                       else "not a plug the engine may switch")
            p.retried = True  # counts as a failed command: the plug did not open
            self.on_command(p.key, action, False, t)
            return
        p.pending, p.pending_at = action, t
        self.commands.append((t, p.key, action))
        self.act.switch(p.key, action, t)

    # ---- people ------------------------------------------------------------------------
    def command(self, req: dict[str, Any], t: float) -> dict[str, Any]:
        out = self._guarded(str(req.get("plug") or "").strip(), t, self._command, req, t)
        return out or {"result": "refused", "reason": "Power protection hit an error. It is stopped on this circuit."}

    def _command(self, req: dict[str, Any], t: float) -> dict[str, Any]:
        kind = req.get("kind")
        key = str(req.get("plug") or "").strip()
        if key not in self.plugs:
            key = key.lower()
        rid = str(req.get("id") or "")[:64]
        by = str(req.get("by") or "")[:80]
        c = self.circuit_of(key)
        p = self.plugs.get(key)
        if c is None or p is None or c.state == "OFF":
            ok, reason = False, "This plug is not on a protected circuit."
        elif kind == "power_on":
            ok, reason = c.power_on(t, p)
        elif kind == "retry":
            ok, reason = c.retry(t, p)
        else:
            ok, reason = False, "Unknown request."
        out = {"result": "done" if ok else "refused", "reason": reason}
        self._entry(t, "request_done" if ok else "request_refused",
                         c.cfg.id if c else None, key or None, c.event if c and c.event else None,
                         {"request": rid, "kind": kind, "by": by, "reason": reason})
        self._entry(t, "person", c.cfg.id if c else None, key or None, None,
                         {"request": rid, "kind": kind, "by": by, "answer": out["result"]})
        return out

    # ---- what the link reads --------------------------------------------------------------
    def box(self, c: Circuit, t: float) -> dict[str, Any] | None:
        st = c.state
        if not self.switching_allowed and st not in ("OFF",):
            return {"state": "switching_off"}
        if st == "SHED":
            return {"state": "shedding"}
        if st == "LOCKOUT":
            return {"state": "checking", "plugs": [p.key for p in c.fired]}
        if st == "BLIND":
            return {"state": "no_power" if c.blind_reason == "no_power" else "offline_event"}
        if st == "OVER":
            failed = [p.key for p in c.plugs.values() if p.failed_open]
            return {"state": "failed_open" if failed else "cant_protect", "plugs": failed}
        if st == "RESTORE":
            nxt = [p.key for p in c.waiting() if p.key != c.restoring]
            return {"state": "restoring", "heater": c.restoring, "next": nxt[0] if nxt else None}
        if st == "HOLD":
            floor, printer = c.predicted_floor()
            if floor is not None and t < floor:
                return {"state": "waiting_heat", "until_s": round(floor - t),
                        "printer": printer.key if printer else None}
            waiting = c.waiting()
            if any(p.failed_open for p in c.plugs.values()):
                return {"state": "failed_open",
                        "plugs": [p.key for p in c.plugs.values() if p.failed_open]}
            if not waiting:
                held = [p.key for p in c.plugs.values() if p.shed_kind == "heater" and p.gave_up]
                return {"state": "restore_blocked", "plugs": held} if held else {"state": "hold"}
            if c.total(t) > c.cfg.restore_w or c.under_since is None:
                return {"state": "waiting_drop"}
            p = waiting[0]
            left = min(self.tm.restore_hold_s * p.hold_mult, self.tm.max_hold_s) - (t - c.under_since)
            if left <= 0 and p.blocked_logged:
                return {"state": "restore_blocked", "plugs": [p.key]}
            return {"state": "hold", "until_s": max(0, round(left)), "heater": p.key}
        if st == "WATCH" and t - c.closed_at <= self.tm.all_clear_s:
            return {"state": "all_clear",
                    "minutes": round((c.closed_at - c.opened_at) / 60, 1),
                    "lost_print": c.lost_print}
        return None

    def status(self, t: float) -> dict[str, Any]:
        circuits = []
        for c in self.circuits.values():
            circuits.append({
                "id": c.cfg.id,
                "state": c.state,
                "sum_w": round(c.total(t), 1),
                "mean_w": round(c.mean(t) or c.total(t), 1),
                "lines": {"shed_w": c.cfg.shed_w, "restore_w": c.cfg.restore_w,
                          "hard_w": c.cfg.hard_w, "target_w": c.cfg.target_w},
                "plan": c.standby_plan(t),
                "event": ({"n": c.event, "opened_s_ago": round(t - c.opened_at, 1),
                           "rounds": c.rounds} if c.event_open else None),
                "box": self.box(c, t),
                "config_waiting": c.pending is not None,
                "switching_off": not self.switching_allowed,
            })
        return {"circuits": circuits, "plugs": {k: self.plug_status(k, t) for k in self.plugs},
                "feed_ok": t - self.feed_at <= self.tm.feed_stale_s,
                "config_version": self.config_version, "errors": self.config_errors}

    def plug_status(self, key: str, t: float) -> dict[str, Any]:
        p = self._plug(key)
        gaps = p.gaps[-20:]
        return {
            "w": round(Circuit.eff(p, t), 1) if p.at > -1e8 else None,
            "age_s": round(t - p.at, 1) if p.at > -1e8 else None,
            "relay": p.relay if p.available else "unknown",
            "reachable": p.reachable,
            "held_off": p.shed_kind or "",
            "failed_open": p.failed_open,
            "learned_w": p.learned_w or None,
            "peak_w": round(p.expected_w(self.tm), 1) if p.role != "other" else None,
            "heartbeat_ok": (len(gaps) >= 3 and max(gaps) <= 2 * self.tm.heartbeat_s + 5)
            if gaps else None,
            "reports_fast": (p_n(gaps, 0.5) <= self.tm.heartbeat_s + 5) if len(gaps) >= 3 else None,
        }

    # ---- storage (survives a Home Assistant restart) ------------------------------------
    STORE_FIELDS = ("shed_kind", "shed_w", "shed_value", "shed_job", "tries", "hold_mult",
                    "gave_up", "failed_open", "hist", "idle_hist", "starts", "relay")

    def to_store(self, t: float) -> dict[str, Any]:
        return {
            "v": 1,
            "engine_id": self.engine_id,
            "config": self.config.get("doc"),
            "seq": self.journal.seq,
            "journal": self.journal.items[-self.journal.cap:],
            "plugs": {k: {**{f: getattr(p, f) for f in self.STORE_FIELDS},
                          "shed_age": round(t - p.shed_at, 1),
                          "restored_age": round(t - p.restored_at, 1)}
                      for k, p in self.plugs.items()},
            "circuits": {cid: {"event": c.event, "event_open": c.event_open,
                               "rounds": c.rounds, "opened_age": round(t - c.opened_at, 1),
                               "lost_print": c.lost_print}
                         for cid, c in self.circuits.items()},
            "followups": {cid: [list(k) for k in v] for cid, v in self.open_followups.items()},
        }

    def from_store(self, data: dict[str, Any], t: float) -> None:
        """Reload after a restart. Nothing fires from saved data: every circuit starts
        in STARTING and waits for fresh readings (failure mode 1)."""
        if not data or data.get("v") != 1:
            return
        self.journal.seq = int(data.get("seq") or 0)
        self.journal.items = list(data.get("journal") or [])
        self.open_followups = {cid: {tuple(k) for k in v}
                               for cid, v in (data.get("followups") or {}).items()}
        for k, d in (data.get("plugs") or {}).items():
            p = self._plug(k)
            for f in self.STORE_FIELDS:
                if f in d:
                    setattr(p, f, d[f])
            p.shed_at = t - float(d.get("shed_age") or 0)
            p.restored_at = t - float(d.get("restored_age") or 1e9)
        if data.get("config"):
            self.apply_config(data["config"], t)
        for cid, d in (data.get("circuits") or {}).items():
            c = self.circuits.get(cid)
            if c is None:
                continue
            c.event = int(d.get("event") or 0)
            c.event_open = bool(d.get("event_open"))
            c.rounds = int(d.get("rounds") or 0)
            c.opened_at = t - float(d.get("opened_age") or 0)
            c.lost_print = bool(d.get("lost_print"))
            if c.state != "OFF":
                c.state, c.started_at = "STARTING", t
        self._entry(t, "engine_started", None, None, None,
                         {"held_off": sorted(k for k, p in self.plugs.items() if p.shed_kind)})
