"""A simulated world for the power engine (DESIGN 12): true loads behind simulated
plugs that report on a 1 W change plus a 30 s heartbeat, relays that can fail or
lie, and a scripted printer feed. Nothing real is switched: this has no path to
Home Assistant. Ported from the design's proposal/sim/run_scenarios.py."""

from __future__ import annotations

import heapq

from custom_components.spoken_parts_power.engine import CircuitCfg, Engine, Plug

DT = 0.25


class World:
    def __init__(self):
        self.loads, self.relay, self.last, self.last_at = {}, {}, {}, {}
        self.report_delay, self.cmd_delay = {}, {}
        self.cmd_fail, self.stuck, self.silent = set(), set(), set()
        self.q, self.n, self.eng = [], 0, None
        self.feed_dead = False

    def add(self, key, load, report_delay=0.5, cmd_delay=0.3):
        self.loads[key], self.relay[key] = load, True
        self.last[key], self.last_at[key] = None, -1e9
        self.report_delay[key], self.cmd_delay[key] = report_delay, cmd_delay

    def at(self, t, fn):
        self.n += 1
        heapq.heappush(self.q, (t, self.n, fn))

    def true_w(self, key, t):
        return self.loads[key](t) if self.relay[key] else 0.0

    # the actuator the engine calls
    def switch(self, key, action, t):
        def done(now):
            if key in self.silent or key in self.cmd_fail:
                return self.eng.on_command(key, action, False, now)
            if key not in self.stuck:
                self.relay[key] = action == "on"
            self.eng.on_command(key, action, True, now)

        self.at(t + self.cmd_delay[key], done)

    def read(self, key, t):
        if key in self.silent:
            return False
        self.at(t + 0.3, lambda now: self.eng.on_power(key, self.true_w(key, now), now))
        return True

    def run(self, eng, until, script=(), start=0.0):
        self.eng = eng
        for when, fn in script:
            self.at(when, fn)
        t = start
        while t <= until:
            for key in self.loads:
                if key in self.silent:
                    continue
                w = self.true_w(key, t)
                if self.last[key] is None or abs(w - self.last[key]) >= 1 or t - self.last_at[key] >= 30:
                    self.last[key], self.last_at[key] = w, t
                    self.at(t + self.report_delay[key], lambda now, k=key, v=w: self.eng.on_power(k, v, now))
            while self.q and self.q[0][0] <= t:
                _, _, fn = heapq.heappop(self.q)
                fn(t)
            for p in eng.plugs.values():  # the link refreshes the feed about once a second
                if p.role == "printer" and not self.feed_dead:
                    p.feed.at = t
            eng.tick(t)
            t = round(t + DT, 2)


def step(*points):
    """Piecewise-constant load: step((0, 15), (60, 1050), (570, 260))."""

    def f(t):
        v = points[0][1]
        for when, val in points:
            if t >= when:
                v = val
        return float(v)

    return f


def cfg(**kw):
    base = dict(id="A", name="Circuit A", shed_w=1440.0, restore_w=1080.0, hard_w=1800.0)
    base.update(kw)
    return CircuitCfg(**base)


def engine(w, plugs, c=None):
    eng = Engine(w)
    eng.add_circuit(c or cfg(), plugs)
    return eng


def farm(w, c=None, p2=step((0, 15), (60, 1050), (570, 260)), ht=165, peak=180, extra=()):
    """Marc's example circuit: P1 printing, AMS HT 1 and 2 heating, P2 starts at t=60."""
    w.add("P1", step((0, 220)))
    w.add("HT1", step((0, ht)))
    w.add("HT2", step((0, ht)))
    w.add("P2", p2)
    plugs = [Plug("P1", "printer"), Plug("HT1", "ams_ht", peak_w=peak), Plug("HT2", "ams_ht", peak_w=peak),
             Plug("P2", "printer")]
    for key, role, load, kw in extra:
        w.add(key, load)
        plugs.append(Plug(key, role, **kw))
    eng = engine(w, plugs, c)
    eng.feed_plug("P1", "printing", 12400, 0)
    eng.feed_plug("P2", "idle", None, 0)
    return eng


def p2_starts(eng, t=60, heat_s=600):
    return (t, lambda now: eng.feed_plug("P2", "starting", 0, now, high_until=now + heat_s))


def p2_prints(eng, t=570):
    return (t, lambda now: eng.feed_plug("P2", "printing", 0, now))


def offs(eng):
    return [(round(t, 2), k) for t, k, a in eng.commands if a == "off"]


def ons(eng):
    return [(round(t, 2), k) for t, k, a in eng.commands if a == "on"]


def kinds(eng, kind):
    return [(t, d) for t, k, d in eng.trace if k == kind]


def followups(eng, kind=None):
    return [(t, d) for t, d in kinds(eng, "followup") if kind is None or d["kind"] == kind]
