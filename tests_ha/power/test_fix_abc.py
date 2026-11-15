"""Fixes A, B and C (ATS-SP-POWER-FIX-1, after the H2D cuts of Nov 15, 2026).

The plugs here report like the real THIRDREALITY plugs on Marc's farm: one reading
about every 5 s, whatever the load does in between. A direct re-read answers in
0.3 s with the value at that moment.

A. Before a printer is cut every plug is re-read, readings older than 5 s are
   dropped, and a printer is cut only if the circuit is still over the shed line.
B. A printer may be cut only after 60 s over the shed line without a break, or
   over the hard line on fresh readings. Heaters keep the fast 2 s trigger.
C. A heater in its heating session counts at its learned draw between pulses.
"""

from __future__ import annotations

import heapq

from custom_components.spoken_parts_power.engine import Plug

from .world import DT, World, engine, kinds, offs, step


class Cadence(World):
    """A world whose plugs report every `gap` seconds (staggered), not on a change."""

    def __init__(self, gap=5.0):
        super().__init__()
        self.gap, self.phase = gap, {}
        self.unanswered, self.muted = set(), set()  # re-reads not answered; reports stopped

    def add(self, key, load, phase=0.0, **kw):
        super().add(key, load, **kw)
        self.phase[key] = phase

    def read(self, key, t):
        if key in self.silent:
            return False
        if key not in self.unanswered:
            self.at(t + 0.3, lambda now: self.eng.on_power(key, self.true_w(key, now), now))
        return True

    def run(self, eng, until, script=(), start=0.0):
        self.eng = eng
        for when, fn in script:
            self.at(when, fn)
        t = start
        while t <= until:
            for key in self.loads:
                if key in self.silent or key in self.muted:
                    continue
                k = (t - self.phase[key]) / self.gap
                if t >= self.phase[key] and abs(k - round(k)) < 1e-6:
                    w = self.true_w(key, t)
                    self.at(t + 0.1, lambda now, kk=key, v=w, m=t: self.eng.on_power(kk, v, now, measured=m))
            while self.q and self.q[0][0] <= t:
                _, _, fn = heapq.heappop(self.q)
                fn(t)
            for p in eng.plugs.values():
                if p.role == "printer" and not self.feed_dead:
                    p.feed.at = t
            eng.tick(t)
            t = round(t + DT, 2)


def ams_cycle(on_w=148.0, off_w=4.2, period=55.0, on_s=25.0, start=0.0, stop=1e9):
    """An AMS HT drying: about 148 W for 25 s, about 4 W for 30 s, until `stop`."""

    def f(t):
        if t < start or t >= stop:
            return 0.0
        return on_w if (t - start) % period < on_s else off_w

    return f


def _learned(eng, key, w=148.0, n=20):
    eng.plugs[key].hist = [w] * n


def _upstairs(w, h2d, x2d, ams):
    """The Upstairs circuit of Nov 15: 15 A, shed 1440, restore 1080, hard 1800, 2 s."""
    w.add("H2D", h2d, phase=0.0)  # phases on the 0.25 s tick grid
    w.add("X2D", x2d, phase=0.5)
    w.add("AMS1", ams, phase=1.25)
    eng = engine(w, [Plug("H2D", "printer"), Plug("X2D", "printer"), Plug("AMS1", "ams_ht")])
    _learned(eng, "AMS1")
    eng.feed_plug("H2D", "idle", None, 0)
    eng.feed_plug("X2D", "idle", None, 0)
    return eng


# ---- the two real cuts -------------------------------------------------------------------


def test_replay_1117_no_printer_is_cut_and_the_cycling_ams_goes_first():
    """11:17 AM, as it happened: AMS-1 reads 148 W, then 4.2 W (off half of its dry
    cycle); the X2D's last report is 318 W from its heat-up tail (it is really at
    68 W); the H2D's calibration heat-up reports 1,206 W. The engine sees 1,528 W
    over the 1,440 W line for 2 s. The old engine skipped AMS-1 as "not drawing",
    kept the 4.6 s old X2D reading and cut the H2D. Now AMS-1 is counted at its
    learned 148 W and goes at once (C); the printers wait (B); the X2D's next
    report is 68 W, the circuit is under the line, and no printer is cut."""
    w = Cadence()
    ams = step((0, 4.2), (25, 148), (55, 4.2), (85, 148), (110, 4.2))
    x2d = step((0, 25), (40, 575), (45, 891), (50, 607), (53, 318), (57, 68))
    h2d = step((0, 50), (57.75, 1206), (60, 1235), (78, 300), (82, 60))
    w.add("H2D", h2d, phase=3.0)
    w.add("X2D", x2d, phase=0.5)
    w.add("AMS1", ams, phase=2.25)
    eng = engine(w, [Plug("H2D", "printer"), Plug("X2D", "printer"), Plug("AMS1", "ams_ht")])
    _learned(eng, "AMS1")
    script = [(30, lambda now: eng.feed_plug("H2D", "starting", 0, now)),
              (30, lambda now: eng.feed_plug("X2D", "starting", 0, now))]
    w.run(eng, 300, script)
    trip = kinds(eng, "trip")[0]
    assert 59.5 < trip[0] < 61, trip
    assert trip[1]["sum"] == 1528 and trip[1]["line"] == "shed", trip
    first = trip[1]["ladder"][0]
    assert first[:3] == ["AMS1", 1, 148] and "between heating pulses" in first[4], first
    assert [k for _, k in offs(eng)] == ["AMS1"], offs(eng)
    assert eng.plugs["H2D"].relay == "on" and eng.plugs["X2D"].relay == "on"
    assert kinds(eng, "printer_wait"), "the printers waited"
    assert not kinds(eng, "sacrifice_decision"), "it never came to choosing a print"


def test_replay_1421_hard_line_on_fresh_readings_still_cuts_the_starting_h2d():
    """2:21 PM: X2D printing at about 620 W (its last report 4.4 s old, really 566 W),
    H2D print heat-up 1,282 W for about 90 s, AMS-1 idle (its dry ended 5 min before).
    Fresh total 1,848 W is over the 1,800 W hard line, so by design the H2D is still
    cut at once (NEEDS-MARC item 1). Fix A: the decision uses the fresh X2D reading."""
    w = Cadence()
    x2d = step((0, 634), (28, 566))  # last report 25.5 s (634 W), really 566 W at the trip
    h2d = step((0, 48), (30, 1282), (120, 300))
    eng = _upstairs(w, h2d, x2d, ams_cycle(stop=0.0))
    eng.plugs["X2D"].drew_at = -1e9
    eng.feed_plug("X2D", "printing", 0, 0)
    script = [(10, lambda now: eng.feed_plug("H2D", "starting", 0, now))]
    w.run(eng, 60, script)
    off = offs(eng)
    assert [k for _, k in off] == ["H2D"], off
    assert off[0][0] < 33, "the hard line still acts at once"
    trip = kinds(eng, "trip")[0][1]
    assert trip["line"] == "hard" and trip["sum"] == 1916, trip  # with the stale 634 W
    dec = kinds(eng, "sacrifice_decision")[0][1]
    assert dec["hard"] is True and dec["answered"] is True
    assert dec["verified_sum"] == 1848, dec  # 1282 + 566 fresh, not the stale 634
    ref = kinds(eng, "refresh")[0][1]
    assert sorted(ref["plugs"]) == ["AMS1", "H2D", "X2D"], "fix A: every plug is re-read"
    assert kinds(eng, "trip")[0][1]["skipped"] == [["AMS1", "not drawing"]], "C: idle AMS is not counted"


def test_replay_1421_with_the_same_load_just_under_the_hard_line_waits_60_s():
    """The same 2:21 PM case 100 W lighter (1,748 W): over the shed line only, so the
    H2D is cut only after 60 s over the line, and its 30 s heat-up is over by then."""
    w = Cadence()
    x2d = step((0, 466))
    h2d = step((0, 48), (30, 1282), (60, 300))
    eng = _upstairs(w, h2d, x2d, ams_cycle(stop=0.0))
    eng.feed_plug("X2D", "printing", 0, 0)
    w.run(eng, 200, [(10, lambda now: eng.feed_plug("H2D", "starting", 0, now))])
    assert not offs(eng), offs(eng)
    assert kinds(eng, "printer_wait"), "the engine says it is waiting"


# ---- A: fresh re-read --------------------------------------------------------------------


def test_A_every_plug_is_re_read_and_a_stale_high_reading_does_not_cut_a_printer():
    """The X2D reported 900 W 2.5 s before the H2D's heat-up report and has dropped to
    60 W since. The engine sees 2,200 W (over the hard line); the re-read sees 60 W,
    the fresh total (1,360 W) is under the shed line, so nothing is cut. Before the
    fix the 2.5 s old reading counted as current and the H2D was cut."""
    w = Cadence(gap=5.0)
    w.add("H2D", step((0, 48), (8, 1300)), phase=0.0)
    w.add("X2D", step((0, 60), (5, 900), (9, 60)), phase=2.5)
    eng = engine(w, [Plug("H2D", "printer"), Plug("X2D", "printer")])
    eng.feed_plug("H2D", "starting", 0, 0)
    eng.feed_plug("X2D", "printing", 100, 0)
    w.run(eng, 200)
    trip = kinds(eng, "trip")[0][1]
    assert trip["line"] == "hard" and trip["sum"] == 2200, trip
    d = kinds(eng, "sacrifice_decision")[0][1]
    assert d["verified_sum"] == 1360 and d["plugs"] == [] and d["outcome"] == "ok", d
    assert not offs(eng), offs(eng)


def test_A_a_plug_that_does_not_answer_counts_only_if_its_reading_is_under_5_s_old():
    """The X2D stops reporting and does not answer the re-read. Its last 700 W is
    4.5 s old at the hard-line trip (it counted as current before the fix) and over
    5 s at the decision: it is dropped. The fresh total is under the shed line, so no
    printer is cut; once it is marked not reachable the circuit is flagged."""
    w = Cadence(gap=5.0)
    w.add("H2D", step((0, 48), (9.5, 1300)), phase=4.5)
    w.add("X2D", step((0, 700)), phase=0.5)
    eng = engine(w, [Plug("H2D", "printer"), Plug("X2D", "printer")])
    eng.feed_plug("H2D", "starting", 0, 0)
    eng.feed_plug("X2D", "printing", 100, 0)
    w.unanswered.add("X2D")

    def mute(now):
        w.muted.add("X2D")

    w.run(eng, 150, [(6.0, mute)])
    trip = kinds(eng, "trip")[0]
    assert trip[1]["line"] == "hard" and 4.0 < trip[0] - 5.5 < 5.0, trip
    decs = kinds(eng, "sacrifice_decision")
    d = decs[0][1]
    assert "X2D" in d["unverified"] and d["answered"] is False and d["waited_s"] >= 1.9, d
    assert d["verified_sum"] == 1300 and d["plugs"] == [], d
    # It answers nothing and reports nothing: no new look until it is marked not
    # reachable (silent 90 s), then a person is asked. No entry loop meanwhile.
    assert [x["outcome"] for _, x in decs] == ["recheck", "unverified_over"], decs
    assert decs[1][0] > 5.5 + 90, decs
    assert not offs(eng)


# ---- B: slow trigger for printers ---------------------------------------------------------


def test_B_heater_goes_at_once_printer_only_after_60_s_over_the_line():
    w = Cadence(gap=5.0)
    w.add("HT", step((0, 150)), phase=0.5)
    w.add("P1", step((0, 700)), phase=1.0)
    w.add("P2", step((0, 15), (20, 900)), phase=2.0)
    eng = engine(w, [Plug("HT", "ams_ht", peak_w=150), Plug("P1", "printer"), Plug("P2", "printer")])
    eng.feed_plug("P1", "printing", 5000, 0)
    eng.feed_plug("P2", "starting", 0, 0)
    w.run(eng, 200)
    off = offs(eng)
    assert [k for _, k in off] == ["HT", "P2"], off
    over_at = 22.1  # P2's first 900 W report arrives
    assert off[0][0] < over_at + 3, "the heater keeps the 2 s trigger"
    assert off[1][0] >= over_at + 60, "the printer waits 60 s"
    assert off[1][0] < over_at + 66
    wait = kinds(eng, "printer_wait")
    assert len(wait) == 1 and wait[0][1]["wait_s"] == 60.0, wait


def test_B_a_dip_under_the_shed_line_starts_the_60_s_over():
    """Over for 50 s, one reading under the line, over for 50 s again: no printer cut."""
    w = Cadence(gap=5.0)
    w.add("P1", step((0, 700)), phase=0.0)
    w.add("P2", step((0, 15), (10, 900), (60, 600), (65, 900), (115, 15)), phase=0.0)
    eng = engine(w, [Plug("P1", "printer"), Plug("P2", "printer")])
    eng.feed_plug("P1", "printing", 5000, 0)
    eng.feed_plug("P2", "starting", 0, 0)
    w.run(eng, 200)
    assert not offs(eng), offs(eng)
    assert len(kinds(eng, "printer_wait")) == 1, "at most one entry every 5 min"


def test_B_hard_line_cuts_a_printer_at_once_on_fresh_readings():
    w = Cadence(gap=5.0)
    w.add("P1", step((0, 800)), phase=0.0)
    w.add("P2", step((0, 15), (10, 1200)), phase=0.0)
    eng = engine(w, [Plug("P1", "printer"), Plug("P2", "printer")])
    eng.feed_plug("P1", "printing", 5000, 0)
    eng.feed_plug("P2", "starting", 0, 0)
    w.run(eng, 40)
    off = offs(eng)
    assert [k for _, k in off] == ["P2"] and off[0][0] < 13, off
    assert kinds(eng, "trip")[0][1]["line"] == "hard"


# ---- C: cycling heater -------------------------------------------------------------------


def test_C_cycling_heater_between_pulses_is_in_the_line_up_at_its_learned_draw():
    w = Cadence(gap=5.0)
    w.add("HT", ams_cycle(period=55.0, on_s=25.0), phase=0.0)
    eng = engine(w, [Plug("HT", "ams_ht")])
    _learned(eng, "HT", 148.0)
    w.run(eng, 40)  # last reading at t=40 is 4.2 W (off half), last pulse at t=20
    c = eng.circuits["A"]
    assert eng.plugs["HT"].w == 4.2
    cands, skipped = c.ladder(40.25)
    assert [(x.plug.key, x.tier, x.w) for x in cands] == [("HT", 1, 148.0)], (cands, skipped)
    assert "between heating pulses" in cands[0].note


def test_C_heater_whose_session_ended_is_not_drawing():
    w = Cadence(gap=5.0)
    w.add("HT", ams_cycle(stop=30.0), phase=0.0)
    eng = engine(w, [Plug("HT", "ams_ht")])
    _learned(eng, "HT", 148.0)
    w.run(eng, 300)
    cands, skipped = eng.circuits["A"].ladder(300.25)
    assert not cands and skipped == [("HT", "not drawing")]


def test_C_heater_without_a_learned_draw_is_still_skipped_when_idle():
    w = Cadence(gap=5.0)
    w.add("HT", step((0, 4.0)), phase=0.0)
    eng = engine(w, [Plug("HT", "dryer")])
    w.run(eng, 20)
    cands, skipped = eng.circuits["A"].ladder(20.25)
    assert not cands and skipped == [("HT", "not drawing")]


# ---- from the independent review ----------------------------------------------------------


def test_review_1_no_cut_decisions_do_not_use_up_the_rounds():
    """A real 1,700 W overload whose reading spikes over the hard line every 10 s.
    Each spike trips the hard line, the fresh re-read is under it, and the engine
    waits. Those decisions cut nothing, so they must not use up the 3 rounds: after
    60 s over the shed line the cheaper print is cut."""
    w = Cadence(gap=5.0)
    w.add("P1", step((0, 700)), phase=0.5)
    w.add("P2", lambda t: 1150.0 if (t % 10) < 0.5 and t >= 10 else 1000.0, phase=0.0)
    eng = engine(w, [Plug("P1", "printer"), Plug("P2", "printer")])
    eng.feed_plug("P1", "printing", 5000, 0)
    eng.feed_plug("P2", "printing", 900, 0)
    w.run(eng, 120)
    off = offs(eng)
    assert [k for _, k in off] == ["P2"] and off[0][0] < 70, off
    assert eng.circuits["A"].state != "OVER"
    assert not [d for _, d in kinds(eng, "cant_protect")]


def test_review_2_a_missed_re_read_is_looked_at_again_not_latched_over():
    """A real 1,700 W overload. P1 reports every 5.5 s and never answers a re-read, so
    at some decisions its reading is just over 5 s old. The engine looks again instead
    of going to OVER, and the print is cut at every phase."""
    for ph in (0.0, 1.0, 2.0, 3.0, 4.0, 5.0):
        w = Cadence(gap=5.5)
        w.add("P1", step((0, 700)), phase=ph)
        w.add("P2", step((0, 60), (5, 1000)), phase=0.25)
        w.unanswered.add("P1")
        eng = engine(w, [Plug("P1", "printer"), Plug("P2", "printer")])
        eng.feed_plug("P1", "printing", 5000, 0)
        eng.feed_plug("P2", "printing", 900, 0)
        w.run(eng, 200)
        assert [k for _, k in offs(eng)] == ["P2"], (ph, offs(eng), kinds(eng, "sacrifice_decision"))
        assert eng.circuits["A"].state != "OVER", ph


def test_review_2_a_plug_that_cannot_be_reached_still_flags_the_circuit():
    """The old rule stays for a plug the engine cannot reach at all: no print is cut on
    its old reading, and a person is asked (OVER, cant_protect)."""
    w = Cadence(gap=5.0)
    w.add("P1", step((0, 700)), phase=0.5)
    w.add("P2", step((0, 60), (5, 1000)), phase=0.0)
    eng = engine(w, [Plug("P1", "printer"), Plug("P2", "printer")])
    eng.feed_plug("P1", "printing", 5000, 0)
    eng.feed_plug("P2", "printing", 900, 0)

    def lose(now):
        w.silent.add("P1")
        eng.on_reachable("P1", False, now)

    w.run(eng, 200, [(7, lose)])
    assert not offs(eng)
    assert eng.circuits["A"].state == "OVER"
    assert any(d["outcome"] == "unverified_over" for _, d in kinds(eng, "sacrifice_decision"))


def test_review_3_hovering_at_the_line_writes_few_printer_wait_entries():
    w = Cadence(gap=5.0)
    w.add("P1", step((0, 700)), phase=0.0)
    w.add("P2", lambda t: 760.0 if int(t // 5) % 2 else 700.0, phase=0.0)
    eng = engine(w, [Plug("P1", "printer"), Plug("P2", "printer")])
    eng.feed_plug("P1", "printing", 5000, 0)
    eng.feed_plug("P2", "printing", 900, 0)
    w.run(eng, 3600)
    assert not offs(eng)
    assert len(kinds(eng, "printer_wait")) <= 13, len(kinds(eng, "printer_wait"))


def test_review_2b_slow_reporter_that_never_answers_is_not_phase_locked():
    """A real 1,700 W overload. P1 reports every 6, 7 or 8 s and never answers a
    re-read. The print is cut at every phase; the circuit never latches OVER."""
    for gap in (6.0, 7.0, 8.0):
        for ph in [x * 0.5 for x in range(int(gap * 2))]:
            w = Cadence(gap=gap)
            w.add("P1", step((0, 700)), phase=ph)
            w.add("P2", step((0, 60), (5, 1000)), phase=0.25)
            w.unanswered.add("P1")
            eng = engine(w, [Plug("P1", "printer"), Plug("P2", "printer")])
            eng.feed_plug("P1", "printing", 5000, 0)
            eng.feed_plug("P2", "printing", 900, 0)
            w.run(eng, 200)
            assert [k for _, k in offs(eng)] == ["P2"], (gap, ph, kinds(eng, "sacrifice_decision"))
            assert eng.circuits["A"].state != "OVER", (gap, ph)


def test_review_4_a_printing_plug_lost_from_zigbee_does_not_loop_the_journal():
    """P2 (printing, 900 W) drops off Zigbee; its old reading keeps the sum over the
    line. No print is cut on it (rule 5) and the circuit is flagged once; the engine
    does not trip again every few seconds while nothing changes."""
    w = Cadence(gap=5.0)
    w.add("P1", step((0, 700)), phase=0.5)
    w.add("P2", step((0, 900)), phase=1.0)
    w.add("P3", step((0, 80)), phase=1.5)
    eng = engine(w, [Plug("P1", "printer"), Plug("P2", "printer"), Plug("P3", "printer")])
    eng.feed_plug("P1", "printing", 5000, 0)
    eng.feed_plug("P2", "printing", 900, 0)
    eng.feed_plug("P3", "idle", None, 0)

    def drop(now):
        w.silent.add("P2")
        eng.on_reachable("P2", False, now)

    w.run(eng, 3600, [(10, drop)])
    assert not offs(eng)
    assert eng.circuits["A"].state == "OVER"
    assert len(kinds(eng, "trip")) <= 2, len(kinds(eng, "trip"))
    assert len(eng.journal.items) < 100, len(eng.journal.items)


def test_recheck_from_over_stays_over():
    """A recheck that starts in OVER goes back to OVER (it never re-enables print cuts)."""
    w = Cadence(gap=5.0)
    w.add("P1", step((0, 700)), phase=0.5)
    w.add("P2", step((0, 900)), phase=1.0)
    w.add("P3", step((0, 80)), phase=1.5)
    eng = engine(w, [Plug("P1", "printer"), Plug("P2", "printer"), Plug("P3", "printer")])
    eng.feed_plug("P1", "printing", 5000, 0)
    eng.feed_plug("P2", "printing", 900, 0)
    eng.feed_plug("P3", "idle", None, 0)
    w.run(eng, 20)
    c = eng.circuits["A"]
    c.state, c.over_since = "OVER", 0.0
    w.unanswered.add("P1")
    w.muted.add("P1")
    w.run(eng, 100, start=20.25)
    assert c.state == "OVER"
    assert not [k for _, k in offs(eng) if k in ("P1", "P2")]
