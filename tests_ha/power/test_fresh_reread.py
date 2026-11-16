"""Fixes A and C and the review regressions, on the ladder mechanics (legacy test timing,
see world.LEGACY). The breaker model itself is in test_breaker_model.py.

Fixes A, B and C (ATS-SP-POWER-FIX-1, after the H2D cuts of Nov 15, 2026).

The plugs here report like the real THIRDREALITY plugs on Marc's farm: one reading
about every 5 s, whatever the load does in between. A direct re-read answers in
0.3 s with the value at that moment.

A. Before a printer is cut every plug is re-read, readings older than 5 s are
   dropped, and a printer is cut only if the circuit is still over the shed line.
B. (Marc, Q-41) No slower trigger. Before a printer is cut each plug is asked for
   its draw right now (a direct Zigbee read); the cut happens only if the circuit
   is really over the line on those readings.
C. A heater in its heating session counts at its learned draw between pulses.
"""

from __future__ import annotations

from custom_components.spoken_parts_power.engine import Plug

from .world import Cadence, ams_cycle, engine, kinds, learned, offs, step


def _upstairs(w, h2d, x2d, ams):
    """The Upstairs circuit of Nov 15: 15 A, shed 1440, restore 1080, hard 1800, 2 s."""
    w.add("H2D", h2d, phase=0.0)  # phases on the 0.25 s tick grid
    w.add("X2D", x2d, phase=0.5)
    w.add("AMS1", ams, phase=1.25)
    eng = engine(w, [Plug("H2D", "printer"), Plug("X2D", "printer"), Plug("AMS1", "ams_ht")])
    learned(eng, "AMS1")
    eng.feed_plug("H2D", "idle", None, 0)
    eng.feed_plug("X2D", "idle", None, 0)
    return eng


# ---- the two real cuts -------------------------------------------------------------------


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
    assert trip["sum"] == 2200, trip
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
    assert 4.0 < trip[0] - 5.5 < 5.0, trip
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


def test_B_heater_first_then_the_printer_on_fresh_readings_without_waiting():
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
    assert off[0][0] < 22.1 + 3 and off[1][0] - off[0][0] <= 1.0, "same round, no slower trigger"
    assert not kinds(eng, "printer_wait")


def test_B_every_plug_is_asked_and_the_answers_are_waited_for():
    """The decision waits for every plug's direct read (here 0.3 s), not a fixed delay."""
    w = Cadence(gap=5.0)
    w.add("P1", step((0, 700)), phase=0.0)
    w.add("P2", step((0, 15), (10, 900)), phase=0.0)
    eng = engine(w, [Plug("P1", "printer"), Plug("P2", "printer")])
    eng.feed_plug("P1", "printing", 5000, 0)
    eng.feed_plug("P2", "starting", 0, 0)
    w.run(eng, 40)
    ref = kinds(eng, "refresh")[0]
    d = kinds(eng, "sacrifice_decision")[0]
    assert sorted(ref[1]["plugs"]) == ["P1", "P2"]
    assert d[1]["answered"] is True and d[0] - ref[0] <= 0.5, (ref, d)
    assert [k for _, k in offs(eng)] == ["P2"]


# ---- C: cycling heater -------------------------------------------------------------------


def test_C_cycling_heater_between_pulses_is_in_the_line_up_at_its_learned_draw():
    w = Cadence(gap=5.0)
    w.add("HT", ams_cycle(period=55.0, on_s=25.0), phase=0.0)
    eng = engine(w, [Plug("HT", "ams_ht")])
    learned(eng, "HT", 148.0)
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
    learned(eng, "HT", 148.0)
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
    """A real 1,700 W overload whose reading also spikes over the hard line every 10 s.
    It is cut on the fresh readings and the circuit never ends in OVER (decisions
    that cut nothing do not use up the 3 rounds)."""
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
    w.add("P2", step((0, 60), (30, 1000)), phase=0.0)
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
    """P2 (printing, 600 W) drops off Zigbee; then P1 rises and its old reading keeps
    the sum over the line. No print is cut on it (rule 5) and the circuit is flagged
    once; the engine does not trip again every few seconds while nothing changes."""
    w = Cadence(gap=5.0)
    w.add("P1", step((0, 700), (30, 900)), phase=0.5)
    w.add("P2", step((0, 600)), phase=1.0)
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
    w.add("P2", step((0, 600), (40, 900)), phase=1.0)
    w.add("P3", step((0, 80)), phase=1.5)
    eng = engine(w, [Plug("P1", "printer"), Plug("P2", "printer"), Plug("P3", "printer")])
    eng.feed_plug("P1", "printing", 5000, 0)
    eng.feed_plug("P2", "printing", 900, 0)
    eng.feed_plug("P3", "idle", None, 0)
    w.run(eng, 30)
    c = eng.circuits["A"]
    c.state = "OVER"
    w.unanswered.add("P1")
    w.muted.add("P1")
    w.run(eng, 100, start=30.25)
    assert any(d["outcome"] == "recheck" for _, d in kinds(eng, "sacrifice_decision"))
    assert c.state == "OVER"
    assert not [k for _, k in offs(eng) if k in ("P1", "P2")]


def test_review_5_the_gate_never_holds_back_an_overload_the_other_plugs_show():
    """P1 stops reporting and answering; a recheck waits for it. Then P3 starts and the
    plugs that can be checked alone are over the hard line (1,900 W): the starting
    print is cut at once, not after P1 is marked silent."""
    w = Cadence(gap=5.0)
    w.add("P1", step((0, 700)), phase=0.5)
    w.add("P2", step((0, 60), (60, 1000)), phase=0.0)
    w.add("P3", step((0, 15), (80, 900)), phase=0.25)
    eng = engine(w, [Plug("P1", "printer"), Plug("P2", "printer"), Plug("P3", "printer")])
    eng.feed_plug("P1", "printing", 5000, 0)
    eng.feed_plug("P2", "printing", 900, 0)
    eng.feed_plug("P3", "idle", None, 0)

    def stop(now):
        w.muted.add("P1")
        w.unanswered.add("P1")

    def start(now):
        eng.feed_plug("P3", "starting", 0, now)

    w.run(eng, 200, [(46, stop), (79, start)])
    assert any(d["outcome"] == "recheck" for _, d in kinds(eng, "sacrifice_decision"))
    off = offs(eng)
    assert [k for _, k in off] == ["P3"] and off[0][0] < 90, off


