"""The breaker model (Marc, STEER-2, Nov 15, 2026): cut only when the breaker is really
close to tripping. Production timers. Plugs report about every 5 s (Cadence), as the
real THIRDREALITY plugs do; a direct re-read answers in 0.3 s.

Fast side of real 15 A trip curves (BREAKER-RESEARCH.md, 40 C, cold start, worst of
Square D QO and Siemens QP 15/20 A): 1.10 x about 180 s, 1.25 x 70 s, 1.5 x 27 s,
2 x 11 s, 3 x 4 s.
"""

from __future__ import annotations

import pytest

from custom_components.spoken_parts_power.engine import CircuitCfg, Engine, Plug, Timers

from .world import Cadence, ams_cycle, kinds, learned, offs, step

RATING = 1800.0  # 15 A x 120 V
FAST_SIDE = {1.10: 180, 1.25: 70, 1.5: 27, 2.0: 11, 3.0: 4}


def breaker_engine(w, plugs):
    eng = Engine(w, timers=Timers())
    eng.add_circuit(CircuitCfg(id="A", name="Upstairs", shed_w=1440.0, restore_w=1080.0,
                               hard_w=1800.0), plugs)
    return eng


def two_printers(w, h2d, x2d, extra=()):
    w.add("H2D", h2d, phase=0.0)
    w.add("X2D", x2d, phase=0.5)
    plugs = [Plug("H2D", "printer"), Plug("X2D", "printer")]
    for key, role, load, phase in extra:
        w.add(key, load, phase=phase)
        plugs.append(Plug(key, role))
    eng = breaker_engine(w, plugs)
    eng.feed_plug("H2D", "printing", 4000, 0)
    eng.feed_plug("X2D", "printing", 9000, 0)
    return eng


# ---- the model against the trip curves -----------------------------------------------------


@pytest.mark.parametrize("r", sorted(FAST_SIDE))
def test_the_model_trips_no_later_than_the_fast_side_of_real_breakers(r):
    w = Cadence()
    eng = breaker_engine(w, [Plug("L", "other")])
    c = eng.circuits["A"]
    c.heat = 0.0
    t_model = c.trip_in_s(RATING * r)
    assert t_model <= FAST_SIDE[r] * 1.05, (r, t_model)  # conservative (3 x: within 5 %)
    assert t_model >= FAST_SIDE[r] * 0.75, (r, t_model)  # but not far more nervous


def test_at_or_under_the_rating_a_breaker_never_trips():
    w = Cadence()
    eng = breaker_engine(w, [Plug("L", "other")])
    c = eng.circuits["A"]
    assert c.trip_in_s(RATING) is None and c.danger(RATING) is None
    assert c.danger(RATING * 1.99) is None, "a cold breaker over its rating is not in danger yet"
    assert c.danger(RATING * 2.0) == "instant"


# ---- Marc's cases: nothing is cut -------------------------------------------------------------


def test_two_printers_heating_together_are_not_cut():
    """About 1,950 W (1.08 x) for 90 s, then printing: nothing is cut."""
    w = Cadence()
    eng = two_printers(w, step((0, 60), (20, 1300), (110, 400)), step((0, 60), (20, 650), (110, 500)))
    w.run(eng, 400)
    assert not offs(eng), offs(eng)
    assert not kinds(eng, "trip")


def test_replay_1117_nothing_is_cut():
    """11:17 AM as it happened (stale 318 W X2D, H2D 1,206 W calibration heat-up, AMS-1
    between pulses): 1,528 W is under the 1,800 W rating, so the breaker is not heating."""
    w = Cadence()
    w.add("H2D", step((0, 50), (57.75, 1206), (60, 1235), (78, 300), (82, 60)), phase=3.0)
    w.add("X2D", step((0, 25), (40, 575), (45, 891), (50, 607), (53, 318), (57, 68)), phase=0.5)
    w.add("AMS1", step((0, 4.2), (25, 148), (55, 4.2), (85, 148), (110, 4.2)), phase=2.25)
    eng = breaker_engine(w, [Plug("H2D", "printer"), Plug("X2D", "printer"), Plug("AMS1", "ams_ht")])
    learned(eng, "AMS1")
    eng.feed_plug("H2D", "starting", 0, 0)
    eng.feed_plug("X2D", "starting", 0, 0)
    w.run(eng, 300)
    assert not offs(eng) and not kinds(eng, "trip")
    assert eng.circuits["A"].heat_pct() == 0.0


def test_replay_1421_if_the_h2d_had_stayed_on_nothing_is_cut():
    """2:21 PM: X2D printing about 600 W, H2D print heat-up 1,282 W. The real heat-up
    lasts about 85 s; here 120 s to be safe. 1,848 to 1,916 W is 1.03 to 1.06 x: the
    breaker warms a little, far from a trip. The H2D is not cut."""
    w = Cadence()
    eng = two_printers(w, step((0, 48), (30, 1282), (150, 300)), step((0, 634), (28, 566), (60, 620)))
    eng.feed_plug("H2D", "starting", 0, 0)
    w.run(eng, 400)
    assert not offs(eng), offs(eng)
    assert eng.circuits["A"].heat_pct() < 40


def test_a_short_blip_far_over_the_rating_is_ignored():
    """A 1 s blip to 3,000 W (1.67 x): a breaker does not trip on that."""
    w = Cadence(gap=1.0)
    w.add("P1", step((0, 600), (30, 3000), (31, 600)), phase=0.0)
    eng = breaker_engine(w, [Plug("P1", "printer")])
    eng.feed_plug("P1", "printing", 5000, 0)
    w.run(eng, 100)
    assert not offs(eng) and not kinds(eng, "trip")


# ---- a real overload is cut before the breaker would trip ---------------------------------


@pytest.mark.parametrize("r", [1.25, 1.5])
def test_a_real_overload_is_cut_before_a_fast_breaker_trips(r):
    """Printing printers drawing r x the rating from t = 20 s: the cheaper print is cut
    after the heaters, before the fast side of the trip curve, on fresh readings."""
    w = Cadence()
    load = RATING * r - 900 - 150
    eng = two_printers(w, step((0, 60), (20, load)), step((0, 900)),
                       extra=[("HT", "ams_ht", step((0, 150)), 0.25)])
    eng.plugs["HT"].hist = [150.0] * 20
    w.run(eng, 200)
    off = offs(eng)
    assert [k for _, k in off][:1] == ["HT"], off  # heaters first
    assert "H2D" in [k for _, k in off], off  # the cheaper print (value 4,000 vs 9,000)
    cut = next(t for t, k in off if k == "H2D")
    assert cut - 20 < FAST_SIDE[r], (r, cut)
    assert kinds(eng, "refresh"), "every plug was read again first"


def test_twice_the_rating_is_cut_at_once():
    w = Cadence()
    eng = two_printers(w, step((0, 60), (20, 1900)), step((0, 900)),
                       extra=[("P3", "printer", step((0, 60), (20, 900)), 0.75)])
    eng.feed_plug("P3", "printing", 2000, 0)
    w.run(eng, 60)
    off = offs(eng)
    assert off and off[0][0] - 20 < 7, off  # one report, two looks, the re-read
    assert kinds(eng, "trip")[0][1]["line"] == "instant"


def test_back_to_back_heat_ups_add_up():
    """1.3 x for 30 s, then 0.5 x for 30 s, again and again: one surge is fine, but the
    breaker keeps the heat, and the surges together would trip it."""
    w = Cadence()
    surge = lambda t: 1440.0 if (t % 60) < 30 else 0.0  # noqa: E731
    eng = two_printers(w, surge, step((0, 900)))
    w.run(eng, 600)
    off = offs(eng)
    assert off, "repeated surges are cut in the end"
    assert off[0][0] > 60, "not on the first surge"


def test_the_fresh_re_read_still_decides():
    """The breaker is warm from a real overload; then the overload ends between the
    trip and the decision: on the fresh readings it is under the rating, nothing is cut."""
    w = Cadence(gap=5.0)
    eng = two_printers(w, step((0, 60), (5, 1700), (45, 200)), step((0, 900)))
    w.run(eng, 120)
    c = eng.circuits["A"]
    for _, d in kinds(eng, "sacrifice_decision"):
        if d["plugs"]:
            assert d["verified_sum"] > RATING, d
    assert all(d["ratio"] > 1.0 for _, d in kinds(eng, "trip")), kinds(eng, "trip")
    assert c.heat_pct() < 100


def test_a_cycling_ams_ht_is_the_first_to_go_in_a_real_overload():
    """C with the breaker model: the AMS HT between pulses (4 W) still goes first."""
    w = Cadence()
    eng = two_printers(w, step((0, 60), (20, 1500)), step((0, 900)),
                       extra=[("AMS1", "ams_ht", ams_cycle(period=55.0, on_s=25.0), 1.25)])
    learned(eng, "AMS1")
    w.run(eng, 200)
    off = offs(eng)
    assert off and off[0][1] == "AMS1", off
