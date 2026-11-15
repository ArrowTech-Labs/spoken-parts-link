"""The design's scenarios (DESIGN 12) as product tests against the real engine core,
with Marc's Nov 15 answers: no shadow, no levels, 4C restore."""

from custom_components.spoken_parts_power.engine import Plug

from .world import (
    World,
    cfg,
    engine,
    farm,
    followups,
    kinds,
    offs,
    ons,
    p2_prints,
    p2_starts,
    step,
)


def test_s01_marc_flow():
    """P1 printing, HT1+HT2 heating, P2 starts. HTs shed once, back after the predicted 10 min and the measured drop."""
    w = World()
    eng = farm(w)
    w.run(eng, 1000, [p2_starts(eng), p2_prints(eng)])
    off, on = offs(eng), ons(eng)
    assert sorted(k for _, k in off) == ["HT1", "HT2"], off
    assert len({t for t, _ in off}) == 1, "both in one shot"
    assert len(kinds(eng, "trip")) == 1
    assert [k for _, k in on] == ["HT1", "HT2"], on
    assert on[0][0] >= 60 + 600, "not before the predicted time"
    assert on[0][0] >= 570 + 120, "not before the measured drop has held"
    assert on[1][0] - on[0][0] >= 90, "one at a time"
    assert kinds(eng, "all_clear") and eng.circuits["A"].state == "WATCH"
    assert kinds(eng, "event_open") and kinds(eng, "event_close")
    assert not followups(eng, "sacrificed"), "no print was touched"


def test_s02_cascade_stale_watts():
    """The HT plugs' reports arrive 6 s late: stale 165 W readings land after the shed. No second pass."""
    w = World()
    eng = farm(w)
    w.report_delay["HT1"] = w.report_delay["HT2"] = 6.0

    def late(now):  # each HT keeps sending its old 165 W for a while
        for k in ("HT1", "HT2"):
            for d in (0.2, 0.9, 1.7, 2.6, 4.0):
                w.at(now + d, lambda n, k=k: eng.on_power(k, 165.0, n))

    w.run(eng, 200, [p2_starts(eng), (63.0, late)])
    off = offs(eng)
    assert sorted(k for _, k in off) == ["HT1", "HT2"], off
    assert len(kinds(eng, "trip")) == 1, "no second pass from stale data"
    assert eng.plugs["P1"].relay == "on" and eng.plugs["P2"].relay == "on"
    assert not kinds(eng, "failed_open"), "late stale reports are not evidence of a failed open"


def test_s03_lowest_value_printer():
    """No heaters. Three printing printers and an unswitchable load. Lowest values go, P1 stays."""
    w = World()
    w.add("P1", step((0, 300)))
    w.add("P3", step((0, 280)))
    w.add("P5", step((0, 300)))
    w.add("HEAT", step((0, 0), (40, 900)))
    eng = engine(w, [Plug("P1", "printer"), Plug("P3", "printer"), Plug("P5", "printer"), Plug("HEAT", "other")])
    eng.feed_plug("P1", "printing", 12400, 0)
    eng.feed_plug("P3", "printing", 1840, 0)
    eng.feed_plug("P5", "printing", 6000, 0)
    w.run(eng, 120)
    off = offs(eng)
    assert sorted(k for _, k in off) == ["P3", "P5"], off
    assert eng.plugs["P1"].relay == "on"
    assert not ons(eng), "a sacrificed printer is never turned back on by the engine"
    assert sorted(d["plug"] for _, d in followups(eng, "sacrificed")) == ["P3", "P5"]


def test_s04_zero_deposit_startup():
    """No heaters. P1 and P3 are mid-print, P2 starts and heats. Only P2 (nothing deposited) is cut."""
    w = World()
    w.add("P1", step((0, 400)))
    w.add("P3", step((0, 350)))
    w.add("P2", step((0, 15), (60, 1050)))
    eng = engine(w, [Plug("P1", "printer"), Plug("P3", "printer"), Plug("P2", "printer")])
    eng.feed_plug("P1", "printing", 12400, 0)
    eng.feed_plug("P3", "printing", 1840, 0)
    eng.feed_plug("P2", "idle", None, 0)
    w.run(eng, 200, [p2_starts(eng)])
    off = offs(eng)
    assert [k for _, k in off] == ["P2"], off
    sac = followups(eng, "sacrificed")
    assert sac and sac[0][1]["starting"] is True


def test_s05_restore_needs_headroom():
    """A 400 W dryer was shed. The sum falls under the restore line but the dryer would not fit: no flip-flop."""
    w = World()
    w.add("P1", step((0, 500), (300, 500), (700, 300)))
    w.add("P2", step((0, 15), (60, 800), (200, 500), (700, 300)))
    w.add("DRY", step((0, 400)))
    eng = engine(w, [Plug("P1", "printer"), Plug("P2", "printer"), Plug("DRY", "dryer", peak_w=400)])
    eng.feed_plug("P1", "printing", 5000, 0)
    eng.feed_plug("P2", "idle", None, 0)
    w.run(eng, 1100, [p2_starts(eng, heat_s=120), p2_prints(eng, 200)])
    off, on = offs(eng), ons(eng)
    assert [k for _, k in off] == ["DRY"], off
    assert kinds(eng, "restore_blocked"), "1000 W + 400 W would not land under 1368 W"
    assert len(on) == 1 and on[0][0] >= 700 + 120, on
    assert len(kinds(eng, "trip")) == 1, "never shed twice"


def test_s06_feed_dead():
    """The printer feed is dead. Heaters are still shed. No printer is cut on an unknown state."""
    w = World()
    eng = farm(w, ht=60, p2=step((0, 15), (300, 1250)))
    w.feed_dead = True
    w.run(eng, 400)
    off = offs(eng)
    assert sorted(k for _, k in off) == ["HT1", "HT2"], off
    c = eng.circuits["A"]
    assert c.state == "OVER" and eng.plugs["P2"].relay == "on" and eng.plugs["P1"].relay == "on"
    assert any(d["outcome"] in ("cant_reach_target", "cant_protect") for _, d in kinds(eng, "lockout_end"))
    assert followups(eng, "cant_protect")
    assert eng.box(c, 400)["state"] == "cant_protect"


def test_s07_plug_unverifiable():
    """HT2 stopped reporting (stale 165 W) and cannot be reached. HT1 is shed. No print dies on the unverified watts."""
    w = World()
    eng = farm(w, p2=step((0, 15), (60, 1100)))

    def lose(now):
        w.silent.add("HT2")
        eng.on_reachable("HT2", False, now)

    w.run(eng, 200, [(20, lose), p2_starts(eng)])
    off = offs(eng)
    assert [k for _, k in off] == ["HT1"], off
    dec = kinds(eng, "sacrifice_decision")
    assert dec and dec[0][1]["plugs"] == [] and "HT2" in dec[0][1]["unverified"], dec
    assert dec[0][1]["outcome"] == "unverified_over" and eng.circuits["A"].state == "OVER"
    assert eng.plugs["P2"].relay == "on"


def test_s08_relay_fails_to_open():
    """HT1's off command fails twice. Its watts count again. The next round, on fresh data, takes the next in line."""
    w = World()
    eng = farm(w, p2=step((0, 15), (60, 1100)))
    w.cmd_fail.add("HT1")
    w.run(eng, 200, [p2_starts(eng)])
    off = offs(eng)
    trips = kinds(eng, "trip")
    assert kinds(eng, "failed_open") and len(trips) == 2, (len(trips), off)
    lock_end = kinds(eng, "lockout_end")[0][0]
    assert trips[1][0] >= lock_end, "round 2 only after the lockout"
    assert [k for _, k in off if k not in ("HT1", "HT2")] == ["P2"], off
    assert eng.plugs["P1"].relay == "on"
    assert followups(eng, "failed_open")


def test_s09_relay_acks_but_stays_on():
    """HT1 confirms off but keeps drawing. Found by the re-read, not by stale data."""
    w = World()
    eng = farm(w, p2=step((0, 15), (60, 1100)))
    w.stuck.add("HT1")
    w.run(eng, 200, [p2_starts(eng)])
    fo = kinds(eng, "failed_open")
    assert fo and fo[0][1]["plug"] == "HT1", fo
    assert fo[0][0] - kinds(eng, "trip")[0][0] >= 5, "not before the verify window"
    assert eng.plugs["P1"].relay == "on"


def test_s10_double_trip_in_lockout():
    """A second load arrives 1 s into the lockout. Nothing is evaluated until it ends, then one more shot."""
    w = World()
    eng = farm(w, p2=step((0, 15), (60, 900)), extra=[("P3", "printer", step((0, 200), (63.5, 450)), {})])
    eng.feed_plug("P3", "printing", 1840, 0)
    w.run(eng, 200, [p2_starts(eng)])
    trips, lock = kinds(eng, "trip"), kinds(eng, "lockout_end")
    assert len(trips) == 2, len(trips)
    assert not [c for c in eng.commands if trips[0][0] < c[0] < lock[0][0]], "no command inside the lockout"
    assert trips[1][0] >= lock[0][0] + 2.0 - 1e-6, "needs a full fresh sustain window"
    second = [k for t, k in offs(eng) if t >= trips[1][0]]
    assert second == ["P2"], second


def test_s11_breaker_trip_and_return():
    """Mid-event the whole circuit goes dark for 5 min. Engine goes blind, sends nothing, then resumes."""
    w = World()
    eng = farm(w)

    def dark(now):
        for k in w.loads:
            w.silent.add(k)
            eng.on_reachable(k, False, now)
        w.loads["P2"] = step((0, 15))
        w.loads["P1"] = step((0, 15))
        eng.feed_plug("P1", "unknown", None, now)
        eng.feed_plug("P2", "unknown", None, now)

    def back(now):
        for k in list(w.silent):
            w.silent.discard(k)
            eng.on_reachable(k, True, now)
        eng.feed_plug("P1", "idle", None, now)
        eng.feed_plug("P2", "idle", None, now)

    w.run(eng, 900, [p2_starts(eng), (120, dark), (420, back)])
    blind = kinds(eng, "blind")
    assert blind and blind[0][1]["reason"] == "no_power"
    assert not [c for c in eng.commands if 120 <= c[0] < 420], "nothing is switched while blind"
    on = ons(eng)
    assert [k for _, k in on] == ["HT1", "HT2"] and on[0][0] >= 420 + 120, on
    nop = followups(eng, "no_power")
    assert [d["open"] for _, d in nop] == [True, False]


def test_s12_hard_line_is_instant_and_blips_are_ignored():
    """A 1 s blip over the shed line does nothing. A jump over the breaker's rating acts on the next look."""
    w = World()
    w.add("P1", step((0, 600), (30, 1500), (31, 600), (80, 1900)))
    w.add("HT1", step((0, 165)))
    eng = engine(w, [Plug("P1", "printer"), Plug("HT1", "ams_ht", peak_w=180)])
    eng.feed_plug("P1", "printing", 5000, 0)
    w.run(eng, 100)
    trips = kinds(eng, "trip")
    assert len(trips) == 1 and 80 <= trips[0][0] <= 81, trips


def test_s15_flip_flop_brake():
    """A restored heater is shed again by a new printer start. Its next wait is twice as long."""
    w = World()
    eng = farm(w, p2=step((0, 15), (60, 1050), (200, 260)),
               extra=[("P3", "printer", step((0, 15), (520, 800), (700, 250)), {})])
    eng.feed_plug("P3", "idle", None, 0)
    w.run(eng, 1500, [p2_starts(eng, heat_s=140), p2_prints(eng, 200),
                      (520, lambda now: eng.feed_plug("P3", "starting", 0, now, high_until=now + 180)),
                      (700, lambda now: eng.feed_plug("P3", "printing", 0, now))])
    on = ons(eng)
    assert eng.plugs["HT1"].hold_mult == 2, eng.plugs["HT1"].hold_mult
    second = [t for t, k in on if k == "HT1"][1]
    assert second >= 700 + 240, (second, on)


# ---- Marc's answers replace s13 (shadow) and s14 (levels) -------------------------------


def test_configured_circuit_protects_at_once():
    """No shadow, no levels, no go-live step: protect on + plugs = sheds on the first trip,
    and may cut a printing printer when the heaters are not enough."""
    w = World()
    eng = farm(w, ht=60, p2=step((0, 15), (60, 1250)))
    eng.feed_plug("P1", "printing", 500, 0)
    w.run(eng, 200, [p2_starts(eng)])
    off = [k for _, k in offs(eng)]
    assert "HT1" in off and "HT2" in off and "P2" in off, off
    assert eng.plugs["P1"].relay == "on", "the higher-value print survives"


def test_protect_off_switches_nothing():
    w = World()
    eng = farm(w, c=cfg(protect=False))
    w.run(eng, 300, [p2_starts(eng)])
    assert eng.commands == [] and eng.circuits["A"].state == "OFF"
    assert not kinds(eng, "trip")


def test_other_plug_is_counted_but_never_in_the_line_up():
    w = World()
    w.add("FAN", step((0, 1500)))
    w.add("HT1", step((0, 100)))
    eng = engine(w, [Plug("FAN", "other"), Plug("HT1", "ams_ht", peak_w=100)])
    w.run(eng, 60)
    assert [k for _, k in offs(eng)] == ["HT1"]
    assert "FAN" not in [c.plug.key for c in eng.circuits["A"].ladder(60)[0]]
    assert eng.circuits["A"].state == "OVER", "1500 W of fan is still over the line"
    assert followups(eng, "cant_protect")


def test_never_turn_off_printer_is_never_cut():
    w = World()
    w.add("P1", step((0, 900)))
    w.add("P2", step((0, 15), (30, 900)))
    eng = engine(w, [Plug("P1", "printer"), Plug("P2", "printer", eligible=False)])
    eng.feed_plug("P1", "printing", 100, 0)
    eng.feed_plug("P2", "idle", None, 0)
    w.run(eng, 100, [(30, lambda now: eng.feed_plug("P2", "starting", 0, now, high_until=now + 600))])
    assert [k for _, k in offs(eng)] == ["P1"], "P2 is starting (value 0) but may not be turned off"


def test_switching_off_in_home_assistant_switches_nothing():
    w = World()
    eng = farm(w)
    eng.switching_allowed = False
    w.run(eng, 200, [p2_starts(eng)])
    assert eng.commands == []
    assert eng.circuits["A"].state == "OVER" and followups(eng, "cant_protect")


# ---- 4C ----------------------------------------------------------------------------------


def _four_c(w):
    """HT1 heating, P3 idle (40 W), P1 printing (low value). P2 starts big."""
    w.add("HT1", step((0, 200)))
    w.add("P3", step((0, 40)))
    w.add("P1", step((0, 300)))
    w.add("P2", step((0, 15), (60, 1250), (400, 250)))
    eng = engine(w, [Plug("HT1", "ams_ht", peak_w=200), Plug("P3", "printer"), Plug("P1", "printer"),
                     Plug("P2", "printer")])
    eng.feed_plug("HT1", "unknown", None, 0)
    eng.feed_plug("P3", "idle", None, 0)
    eng.feed_plug("P1", "printing", 300, 0)
    eng.feed_plug("P2", "idle", None, 0)
    return eng


def test_4c_idle_printer_comes_back_after_the_heaters_and_printing_one_stays_off():
    w = World()
    eng = _four_c(w)
    script = [(60, lambda now: eng.feed_plug("P2", "starting", 0, now, high_until=now + 300)),
              (400, lambda now: eng.feed_plug("P2", "printing", 900, now))]
    w.run(eng, 1200, script)
    off = sorted(k for _, k in offs(eng))
    # need = 1805 - 1368 = 437: HT1 (200) + P3 (40) are all the free loads, then the
    # lowest value print: P2 is starting (value 0) and goes before P1.
    assert off == ["HT1", "P2", "P3"], off
    assert eng.plugs["P2"].shed_kind == "sacrifice" and eng.plugs["P3"].relay == "on"
    on = [k for _, k in ons(eng)]
    assert on == ["HT1", "P3"], "heater first, then the idle printer; never the printing one"
    assert eng.plugs["P2"].relay == "off"
    assert [d["open"] for _, d in followups(eng, "idle_off")] == [True, False]


def test_4c_person_power_on_is_refused_without_room_and_done_with_room():
    w = World()
    eng = _four_c(w)
    script = [(60, lambda now: eng.feed_plug("P2", "starting", 0, now, high_until=now + 300))]
    w.run(eng, 75, script)
    c = eng.circuits["A"]
    assert eng.plugs["P2"].shed_kind == "sacrifice"
    # Pretend the circuit is busy: P1 now draws a lot, so P2 (expected ~60 W idle) has no room.
    w.loads["P1"] = step((0, 1330))
    w.run(eng, 200, start=75.25)
    res = eng.command({"id": "r1", "kind": "power_on", "plug": "P2", "by": "Marc"}, 200)
    assert res["result"] == "refused" and "not enough room" in res["reason"], res
    assert not [k for _, k in ons(eng) if k == "P2"]
    w.loads["P1"] = step((0, 300))
    w.run(eng, 900, start=200.25)
    assert c.state in ("HOLD", "WATCH")
    res = eng.command({"id": "r2", "kind": "power_on", "plug": "P2", "by": "Marc"}, 900)
    assert res["result"] == "done", res
    w.run(eng, 960, start=900.25)
    assert eng.plugs["P2"].relay == "on" and eng.plugs["P2"].shed_kind is None
    sac = followups(eng, "sacrificed")
    assert [d["open"] for _, d in sac] == [True, False]
    kinds_ = [d["kind"] for _, d in kinds(eng, "person")]
    assert kinds_ == ["power_on", "power_on"]


def test_retry_resets_a_heater_that_used_its_tries():
    w = World()
    eng = farm(w)
    w.run(eng, 70, [p2_starts(eng)])
    p = eng.plugs["HT1"]
    p.tries, p.gave_up = 3, True
    res = eng.command({"id": "r", "kind": "retry", "plug": "HT1"}, 70)
    assert res["result"] == "done" and p.tries == 0 and not p.gave_up
    assert eng.command({"id": "x", "kind": "retry", "plug": "P1"}, 70)["result"] == "refused"
