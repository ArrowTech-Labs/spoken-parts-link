"""Regression tests for the independent review of the engine (Nov 15, 2026).
One test (or more) per finding; the number is the review's."""

from custom_components.spoken_parts_power.engine import Engine, Plug

from .world import (
    World,
    doc,
    doc_engine,
    engine,
    farm,
    followups,
    kinds,
    offs,
    ons,
    p2_starts,
    step,
)


class Act:
    def __init__(self):
        self.switched, self.reads = [], []

    def switch(self, key, action, t):
        self.switched.append((t, key, action))

    def read(self, key, t):
        self.reads.append((t, key))
        return True


# 1 -------------------------------------------------------------------------------------
def test_1_late_high_report_after_confirm_needs_a_reread_and_cuts_no_print():
    """HT reports run 7 s late. Old 165 W readings land more than 5 s after the confirm.
    They make the plug suspect and trigger a re-read; the re-read says 0 W. No failed
    open, no second round, no printer cut."""
    w = World()
    eng = farm(w, p2=step((0, 15), (60, 1050), (570, 260)))
    w.report_delay["HT1"] = w.report_delay["HT2"] = 7.0

    def late(now):
        for k in ("HT1", "HT2"):
            for d in (5.6, 6.2, 6.9):
                w.at(now + d, lambda n, k=k: eng.on_power(k, 165.0, n))

    w.run(eng, 200, [p2_starts(eng), (62.6, late)])
    assert not kinds(eng, "failed_open"), kinds(eng, "failed_open")
    assert len(kinds(eng, "trip")) == 1
    assert sorted(k for _, k in offs(eng)) == ["HT1", "HT2"]
    assert eng.plugs["P1"].relay == "on" and eng.plugs["P2"].relay == "on"


def test_1_report_measured_before_the_confirm_is_ignored():
    act = Act()
    eng = Engine(act)
    eng.apply_config(doc([{"id": "A"}], [{"ieee": "h", "role": "ams_ht", "circuit": "A"}]), 0)
    p = eng.plugs["h"]
    p.relay, p.shed_kind, p.zeroed, p.fired_at, p.confirmed_at = "off", "heater", True, 10, 10.3
    eng.on_power("h", 165, 20, measured=9.9)  # arrives late, measured before the cut
    assert p.zeroed and not p.failed_open and p.suspect_at is None
    eng.on_power("h", 165, 20.1)  # measured late: suspect, re-read asked
    assert p.suspect_at == 20.1 and not p.failed_open and act.reads[-1] == (20.1, "h")
    eng.on_power("h", 0, 20.5)  # the re-read: it is off
    assert not p.zeroed and not p.failed_open and p.suspect_at is None


# 2 -------------------------------------------------------------------------------------
def _ab_world():
    w = World()
    w.add("h1", step((0, 300)))
    w.add("h2", step((0, 300)))
    w.add("x", step((0, 1000), (30, 1300), (90, 200), (200, 1500)))
    plugs = [{"ieee": "h1", "role": "ams_ht", "circuit": "A", "rated_w": 300},
             {"ieee": "h2", "role": "ams_ht", "circuit": "A", "rated_w": 300},
             {"ieee": "x", "role": "other", "circuit": "A"}]
    eng = doc_engine(w, [{"id": "A"}], plugs)
    return w, eng, plugs


def test_2_protect_off_during_the_lockout_is_applied_when_it_ends():
    w, eng, plugs = _ab_world()
    c = eng.circuits["A"]
    seen = {}

    def turn_off(now):
        seen["state"] = c.state
        eng.apply_config(doc([{"id": "A", "protect": False}], plugs, 2), now)
        seen["pending"] = c.pending is not None

    w.run(eng, 400, [(32.5, turn_off)])
    assert seen == {"state": "LOCKOUT", "pending": True}
    assert c.state == "OFF" and c.cfg.protect is False and c.pending is None
    after = [x for x in eng.commands if x[0] > 33]
    assert after == [], "nothing switched once protection is off, not even at 1500 W"


# 3 -------------------------------------------------------------------------------------
def test_3_off_command_times_out_but_the_relay_opens_is_a_late_confirm():
    w = World()
    eng = farm(w)
    real_switch = w.switch

    def switch(key, action, t):
        if key == "HT1" and action == "off":
            def done(now):
                w.relay["HT1"] = False  # it did open...
                eng.on_command("HT1", "off", False, now)  # ...but the call timed out
            w.at(t + 2.0, done)
            w.at(t + 4.5, lambda now: eng.on_relay("HT1", "off", now))  # HA's state report
            return
        real_switch(key, action, t)

    w.switch = switch
    w.run(eng, 1000, [p2_starts(eng), (570, lambda n: eng.feed_plug("P2", "printing", 0, n))])
    p = eng.plugs["HT1"]
    assert [d for _, d in kinds(eng, "shed_confirmed") if d.get("late")]
    assert not kinds(eng, "external_change")
    assert "HT1" in [k for _, k in ons(eng)], "the engine still restores what it shed"
    assert p.relay == "on" and not p.failed_open


# 4 -------------------------------------------------------------------------------------
def test_4_idle_printer_with_a_stale_feed_is_not_cut_as_idle():
    w = World()
    w.add("P3", step((0, 300)))
    w.add("HEAT", step((0, 0), (40, 1300)))
    eng = engine(w, [Plug("P3", "printer"), Plug("HEAT", "other")])
    eng.feed_plug("P3", "idle", None, 0)
    w.feed_dead = True  # the feed stops refreshing: 40 s old at the trip
    w.run(eng, 60)
    assert "P3" not in [k for _, k in offs(eng)]
    sk = [d for _, d in kinds(eng, "cant_protect")]
    assert sk and any(s[0] == "P3" and "not known" in s[1] for s in sk[0]["skipped"])


# 5 -------------------------------------------------------------------------------------
def test_5_blind_and_starting_reset_the_calm_clock_and_hold_window():
    w = World()
    eng = farm(w)
    c = eng.circuits["A"]
    seen = {}

    def dark(now):
        seen["before"] = (c.under_since, len(c.hist))
        for k in w.loads:
            w.silent.add(k)
            eng.on_reachable(k, False, now)

    def look(now):
        seen["blind"] = (c.state, c.under_since, len(c.hist))

    def back(now):
        for k in list(w.silent):
            w.silent.discard(k)
            eng.on_reachable(k, True, now)

    def look2(now):
        seen["after"] = (c.state, c.under_since, min(ts for ts, _ in c.hist))

    w.run(eng, 700, [p2_starts(eng), (570, lambda n: eng.feed_plug("P2", "printing", 0, n)),
                     (620, dark), (640, look), (650, back), (652, look2)])
    assert seen["before"][0] is not None, "the calm clock was running before the dark"
    assert seen["blind"] == ("BLIND", None, 0)
    assert seen["after"][2] >= 650, "nothing from before the dark is in the hold window"
    assert not [k for t, k in ons(eng) if t < 650 + 120]


# 6 -------------------------------------------------------------------------------------
def test_6_event_stays_open_while_a_heater_that_gave_up_is_off_and_retry_restores_it():
    w = World()
    eng = farm(w)
    w.run(eng, 70, [p2_starts(eng), (570, lambda n: eng.feed_plug("P2", "printing", 0, n))])
    eng.plugs["HT1"].tries = 3  # it used its tries
    w.run(eng, 1200, start=70.25)
    c = eng.circuits["A"]
    assert eng.plugs["HT1"].gave_up and eng.plugs["HT1"].relay == "off"
    assert c.event_open and not kinds(eng, "event_close"), "the event waits for a person"
    assert eng.box(c, 1200)["state"] == "restore_blocked"
    res = eng.command({"id": "r", "kind": "retry", "plug": "HT1", "by": "Marc"}, 1200)
    assert res["result"] == "done"
    w.run(eng, 1600, start=1200.25)
    assert eng.plugs["HT1"].relay == "on" and kinds(eng, "event_close")


def test_6_retry_from_watch_puts_the_circuit_back_in_hold():
    w = World()
    eng = farm(w)
    w.run(eng, 70, [p2_starts(eng), (570, lambda n: eng.feed_plug("P2", "printing", 0, n))])
    c = eng.circuits["A"]
    p = eng.plugs["HT1"]
    p.tries, p.gave_up = 3, True
    c.state, c.event_open = "WATCH", False  # as after a restart that lost the event
    w.loads["P2"] = step((0, 260))
    assert eng.command({"id": "r", "kind": "retry", "plug": "HT1"}, 70.25)["result"] == "done"
    assert c.state == "HOLD"
    w.run(eng, 700, start=70.5)
    assert p.relay == "on"


# 7 -------------------------------------------------------------------------------------
def test_7_person_power_on_after_the_event_closed_returns_to_watch():
    w = World()
    w.add("P1", step((0, 300)))
    w.add("P3", step((0, 280)))
    w.add("HEAT", step((0, 0), (40, 1100), (130, 0)))  # fix B: over the line for more than 60 s
    eng = engine(w, [Plug("P1", "printer"), Plug("P3", "printer"), Plug("HEAT", "other")])
    eng.feed_plug("P1", "printing", 12400, 0)
    eng.feed_plug("P3", "printing", 1840, 0)
    w.run(eng, 400)
    c = eng.circuits["A"]
    assert eng.plugs["P3"].shed_kind == "sacrifice" and c.state == "WATCH" and not c.event_open
    eng.feed_plug("P3", "idle", None, 400)
    assert eng.command({"id": "r", "kind": "power_on", "plug": "P3"}, 400)["result"] == "done"
    w.run(eng, 600, start=400.25)
    assert eng.plugs["P3"].relay == "on"
    assert c.state == "WATCH", c.state


# 8 -------------------------------------------------------------------------------------
def test_8_moving_a_plug_waits_until_both_circuits_are_quiet():
    w = World()
    w.add("h1", step((0, 300)))
    w.add("x", step((0, 1000), (30, 1300), (90, 200)))
    w.add("b1", step((0, 100)))
    plugs = [{"ieee": "h1", "role": "ams_ht", "circuit": "A", "rated_w": 300},
             {"ieee": "x", "role": "other", "circuit": "A"},
             {"ieee": "b1", "role": "ams_ht", "circuit": "B"}]
    eng = doc_engine(w, [{"id": "A"}, {"id": "B"}], plugs)
    a, b = eng.circuits["A"], eng.circuits["B"]
    seen = {}

    def move(now):
        moved = [dict(p) for p in plugs]
        moved[0]["circuit"] = "B"
        eng.apply_config(doc([{"id": "A"}, {"id": "B"}], moved, 2), now)
        seen["during"] = ("h1" in a.plugs, "h1" in b.plugs, a.event_open)

    w.run(eng, 1200, [(40, move)])
    assert seen["during"] == (True, False, True), seen
    assert not a.event_open and "h1" in b.plugs and "h1" not in a.plugs


# 9 -------------------------------------------------------------------------------------
def test_9_a_valid_reading_taken_after_it_went_away_marks_the_plug_reachable():
    eng = Engine(Act())
    eng.apply_config(doc([{"id": "A"}], [{"ieee": "h", "role": "ams_ht", "circuit": "A"}]), 0)
    eng.on_reachable("h", False, 10)
    eng.on_power("h", 50, 12, measured=9)
    assert not eng.plugs["h"].reachable, "measured before it went away"
    eng.on_power("h", 50, 13, measured=12.5)
    assert eng.plugs["h"].reachable


# 11 ------------------------------------------------------------------------------------
def test_11_switching_off_makes_no_restore_attempts_and_uses_no_tries():
    w = World()
    eng = farm(w)
    w.run(eng, 70, [p2_starts(eng), (570, lambda n: eng.feed_plug("P2", "printing", 0, n))])
    eng.switching_allowed = False
    w.run(eng, 1000, start=70.25)
    assert not ons(eng) and eng.plugs["HT1"].tries == 0
    assert not kinds(eng, "restore_blocked") and not followups(eng, "restore_blocked")
    assert len(kinds(eng, "switching_off")) == 1
    eng.switching_allowed = True
    w.run(eng, 1400, start=1000.25)
    assert [k for _, k in ons(eng)] == ["HT1", "HT2"]


# 12 ------------------------------------------------------------------------------------
def test_12_an_exception_in_an_off_circuit_tick_or_a_handler_is_a_fault_not_a_crash():
    w = World()
    eng = farm(w)
    c = eng.circuits["A"]
    c.state = "OFF"

    def boom(t):
        raise RuntimeError("x")

    c.tick = boom
    eng.tick(1.0)
    assert kinds(eng, "fault")
    eng2 = farm(World())

    def bad(*a):
        raise ValueError("y")

    eng2._on_command = bad
    eng2.on_command("HT1", "off", True, 2.0)
    assert eng2.circuits["A"].state == "OFF" and kinds(eng2, "fault")


def test_12_restores_strictly_heaters_first():
    """The heater's window is doubled (240 s). The idle printer fits after 120 s but
    waits until the heater's window is over and it went back on."""
    w = World()
    w.add("HT1", step((0, 200)))
    w.add("P3", step((0, 300)))
    w.add("P1", step((0, 300)))
    w.add("P2", step((0, 15), (60, 850), (200, 250)))
    eng = engine(w, [Plug("HT1", "ams_ht", peak_w=200), Plug("P3", "printer"), Plug("P1", "printer"),
                     Plug("P2", "printer")])
    eng.feed_plug("P3", "idle", None, 0)
    eng.feed_plug("P1", "printing", 300, 0)
    eng.feed_plug("P2", "idle", None, 0)
    w.run(eng, 130, [(60, lambda now: eng.feed_plug("P2", "starting", 0, now, high_until=now + 5))])
    assert sorted(k for _, k in offs(eng)) == ["HT1", "P3"]
    eng.plugs["HT1"].hold_mult = 2
    w.run(eng, 900, start=130.25)
    on = ons(eng)
    assert [k for _, k in on] == ["HT1", "P3"], on
    assert on[0][0] >= 200 + 240 - 10, on


def test_12_booked_watts_are_never_verified_readings():
    eng = Engine(Act())
    eng.apply_config(doc([{"id": "A"}], [{"ieee": "h", "role": "ams_ht", "circuit": "A"}]), 0)
    p = eng.plugs["h"]
    p.w, p.at, p.booked_w, p.booked_until = 0.0, 10.0, 500.0, 100.0
    c = eng.circuits["A"]
    assert c.total(11) == 500 and c.total(11, verified_only=True) == 0


# A (cross-repo check) --------------------------------------------------------------------
def test_A_blind_upgrades_to_no_power_when_plugs_fall_silent_one_after_another():
    w = World()
    keys = ["H1", "H2", "H3", "H4", "H5"]
    for k in keys:
        w.add(k, step((0, 100)))
    eng = engine(w, [Plug(k, "ams_ht", peak_w=100) for k in keys])
    script = []
    for i, k in enumerate(keys):  # staggered: 30 s apart
        def gone(now, k=k):
            w.silent.add(k)
            eng.on_reachable(k, False, now)
        script.append((100 + 30 * i, gone))

    def back(now):
        w.silent.discard("H1")
        eng.on_reachable("H1", True, now)

    script.append((400, back))
    w.run(eng, 420, script)
    reasons = [d["reason"] for _, d in kinds(eng, "blind")]
    assert reasons[0] == "lost_contact" and "no_power" in reasons, reasons
    nop = followups(eng, "no_power")
    assert [d["open"] for _, d in nop] == [True, False], nop
    assert nop[0][0] >= 220, "opened only once every plug was gone"
    assert reasons[-1] == "lost_contact"


# B (cross-repo check) --------------------------------------------------------------------
def test_B_rejected_circuit_is_reported_in_status_errors():
    eng = Engine(Act())
    d = doc([{"id": "A"}, {"id": "B", "restore_w": 1500}],
            [{"ieee": "h", "role": "ams_ht", "circuit": "A"}])
    res = eng.apply_config(d, 0)
    assert res["errors"] and "B" not in eng.circuits and "A" in eng.circuits
    st = eng.status(1)
    assert st["config_version"] == 1 and any("restore line" in e for e in st["errors"])
