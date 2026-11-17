"""Make room (Marc, Studio Q-73, Nov 17, 2026) and the hot panel (Q-71). Production
timers; plugs report about every 5 s (Cadence); a direct re-read answers in 0.3 s.
Nothing real is switched: the world is a simulation with fake plugs.

Q-73: "build 'make room' in the power engine, pausing drying AMS units or dryers for a
start, back on after the heat up"; "you don't need to limit it to 2, pause as many as
they need". Never a printer, never a plug marked "never turn off", never other
equipment; nothing paused when pausing all of them would not be enough.
"""

from __future__ import annotations

import pytest

from custom_components.spoken_parts_power.engine import CircuitCfg, Engine, Plug, Timers
from custom_components.spoken_parts_power.engine.core import validate_circuit

from .world import Cadence, ams_cycle, kinds, learned, offs, ons, step

T_ASK = 45.0  # the circuit arms once every plug has reported (about 31 s here)


def upstairs(w, *, x2d=600.0, h2d=None, heaters=(), others=(), hot_panel=False):
    """The Upstairs 15 A circuit: the X2D printing, the H2D idle, then (optionally) the
    H2D heating up once its file is sent. `heaters`: (key, role, load, kw)."""
    w.add("X2D", step((0, x2d)), phase=0.5)
    w.add("H2D", h2d or step((0, 40)), phase=0.0)
    plugs = [Plug("X2D", "printer", label="X2D"), Plug("H2D", "printer", label="H2D P1")]
    for i, (key, role, load, kw) in enumerate(list(heaters) + list(others)):
        w.add(key, load, phase=1.0 + i * 0.75)  # on the 0.25 s grid
        plugs.append(Plug(key, role, label=key, **kw))
    eng = Engine(w, timers=Timers())
    eng.add_circuit(CircuitCfg(id="A", name="Upstairs", shed_w=1440.0, restore_w=1080.0,
                               hard_w=1800.0, hot_panel=hot_panel), plugs)
    eng.feed_plug("X2D", "printing", 9000, 0)
    eng.feed_plug("H2D", "idle", None, 0)
    for key, role, _, _ in heaters:
        if role in ("ams_ht", "dryer"):
            learned(eng, key, 148.0)
    return eng


def ask(eng, need=1320.0, booked=0.0, plug="H2D", rid="r1"):
    return lambda now: eng.command({"id": rid, "kind": "make_room", "plug": plug, "by": "Kim",
                                    "need_w": need, "booked_w": booked}, now)


def answers(eng):
    return [d for _, k, d in eng.trace if k in ("request_done", "request_refused")
            and d.get("kind") == "make_room"]


def ams(key, **kw):
    return (key, "ams_ht", ams_cycle(start=0.0), kw)


# ---- the decision ---------------------------------------------------------------------------


def test_room_already_nothing_is_paused():
    w = Cadence()
    eng = upstairs(w, x2d=250.0, heaters=[ams("AMS1")])
    w.run(eng, 75, [(T_ASK, ask(eng))])
    a = answers(eng)
    assert [x["outcome"] for x in a] == ["room"]
    assert not offs(eng)


def test_short_pauses_the_drying_ams_units_never_a_printer():
    """X2D printing at 600 W, two AMS HT drying (148 W each, between pulses counted at
    their learned draw): 600 + 296 - 40 + 1,320 = 2,176 W > 1,800 W. Short 376 W: pausing
    both AMS (296 W) is not enough, so nothing is paused."""
    w = Cadence()
    eng = upstairs(w, x2d=600.0, heaters=[ams("AMS1"), ams("AMS4")])
    w.run(eng, 75, [(T_ASK, ask(eng))])
    a = answers(eng)
    assert [x["outcome"] for x in a] == ["no_room"]
    assert "Nothing was paused" in a[0]["reason"]
    assert not offs(eng), "nothing is switched when it would not be enough"


def test_pauses_as_many_as_needed_not_only_two():
    """Marc: "you don't need to limit it to 2, pause as many as they need". Three drying
    AMS units and a dryer: the shortfall needs three of them."""
    w = Cadence()
    heaters = [ams("AMS1"), ams("AMS2"), ams("AMS3"), ("DRY1", "dryer", step((0, 148.0)), {})]
    eng = upstairs(w, x2d=300.0, heaters=heaters)
    # 300 + 4 x 148 - 40 + 1,320 = 2,172 W; short 372 W: three of 148 W are needed.
    w.run(eng, 75, [(T_ASK, ask(eng))])
    a = answers(eng)
    assert [x["outcome"] for x in a] == ["room_made"], a
    paused = [k for k, _ in a[0]["paused"]]
    assert len(paused) == 3
    assert set(k for _, k in offs(eng)) == set(paused)
    assert "H2D" not in paused and "X2D" not in paused
    for k in paused:
        assert eng.plugs[k].shed_kind == "share"


def test_never_never_turn_off_never_other_never_a_printer():
    w = Cadence()
    heaters = [ams("AMS1", eligible=False), ("AMS2", "other", step((0, 148.0)), {}),
               ams("AMS4")]
    eng = upstairs(w, x2d=480.0, heaters=heaters)
    eng.plugs["AMS1"].eligible = False  # marked "never turn off"
    # 480 + 148 x 3 - 40 + 1,320 = 2,204: short 404. Only AMS4 may be paused (148 W).
    w.run(eng, 75, [(T_ASK, ask(eng))])
    assert [x["outcome"] for x in answers(eng)] == ["no_room"]
    assert not offs(eng)


def test_only_what_is_needed_smallest_first_like_the_watch():
    w = Cadence()
    heaters = [("DRY1", "dryer", step((0, 300.0)), {}), ams("AMS1")]
    eng = upstairs(w, x2d=320.0, heaters=heaters)
    learned(eng, "DRY1", 300.0)
    # In use 320 + 40 + 300 + 148, minus the H2D's own 40, plus its 1,320 heat up:
    # 2,088 W, short 288. The ladder goes smallest first
    # (AMS1 148, then DRY1 300), then trims what is not needed: DRY1 alone covers it.
    w.run(eng, 75, [(T_ASK, ask(eng))])
    a = answers(eng)
    assert [x["outcome"] for x in a] == ["room_made"]
    assert [k for k, _ in a[0]["paused"]] == ["DRY1"]


def test_refused_while_the_watch_is_busy_or_switching_is_off():
    w = Cadence()
    eng = upstairs(w, x2d=420.0, heaters=[ams("AMS1")])
    eng.switching_allowed = False
    w.run(eng, 75, [(T_ASK, ask(eng))])
    a = answers(eng)
    assert a and a[0]["reason"].startswith("Switching is turned off")
    assert not offs(eng)


def test_unknown_or_bad_requests_change_nothing():
    w = Cadence()
    eng = upstairs(w, heaters=[ams("AMS1")])
    out = []
    w.run(eng, 75, [(T_ASK, lambda now: out.append(eng.command(
        {"id": "x", "kind": "make_room", "plug": "AMS1", "need_w": 1300}, now))),
        (T_ASK + 1, lambda now: out.append(eng.command(
            {"id": "y", "kind": "make_room", "plug": "H2D", "need_w": "lots"}, now)))])
    assert [o["result"] for o in out] == ["refused", "refused"]
    assert not offs(eng)


# ---- coming back ------------------------------------------------------------------------------


def h2d_heats(start):
    """The H2D: idle 40 W, heats up at 1,300 W for 90 s from `start`, then prints at 250 W."""
    return step((0, 40), (start, 1300), (start + 90, 250))


def test_back_on_after_the_heat_up_one_at_a_time():
    w = Cadence()
    heaters = [ams("AMS1"), ams("AMS2"), ams("AMS3")]
    eng = upstairs(w, x2d=300.0, h2d=h2d_heats(75), heaters=heaters)
    w.run(eng, 900, [(T_ASK, ask(eng)),
                     (73, lambda now: eng.feed_plug("H2D", "starting", 0, now))])
    a = answers(eng)
    assert a[0]["outcome"] == "room_made"
    paused = {k for k, _ in a[0]["paused"]}
    rel = kinds(eng, "share_released")
    assert rel and rel[0][1]["why"] == "heated_up", rel
    back = [(t, k) for t, k in ons(eng)]
    assert {k for _, k in back} == paused, "every paused AMS comes back"
    assert all(t > 75 + 90 for t, _ in back), "none before the heat up is over"
    times = sorted(t for t, _ in back)
    assert all(b - a >= 89 for a, b in zip(times, times[1:])), "one at a time"
    for k in paused:
        assert eng.plugs[k].shed_kind is None and eng.plugs[k].relay == "on"
    assert not kinds(eng, "trip"), "the watch had nothing to do"


def test_back_after_ten_minutes_when_the_printer_never_starts():
    w = Cadence()
    eng = upstairs(w, x2d=420.0, heaters=[ams("AMS1"), ams("AMS2"), ams("AMS3")])
    w.run(eng, 900, [(T_ASK, ask(eng, need=1320.0))])
    rel = kinds(eng, "share_released")
    assert rel and rel[0][1]["why"] == "not_started"
    assert 600 <= rel[0][0] - T_ASK <= 605
    assert ons(eng), "the paused heaters come back"
    assert all(t >= T_ASK + 600 for t, _ in ons(eng))


def test_a_dry_that_does_not_pick_up_opens_restart_the_dry_marked_shared():
    """An AMS HT whose dry does not start again by itself after its plug was off."""
    w = Cadence()
    dead = {"on": True}

    def ams_stops(t):
        return ams_cycle()(t) if dead["on"] else 0.0

    heaters = [("AMS1", "ams_ht", ams_stops, {})]
    eng = upstairs(w, x2d=400.0, heaters=heaters)
    # 400 + 40 + 148 - 40 + 1,320 = 1,868 W: short 68 W, AMS1 alone is paused.
    w.run(eng, 900, [(T_ASK, ask(eng)), (T_ASK + 5, lambda now: dead.update(on=False))])
    assert [k for k, _ in answers(eng)[0]["paused"]] == ["AMS1"]
    assert [k for _, k in ons(eng)] == ["AMS1"], "back on after the 10 minutes"
    fu = [d for _, k, d in eng.trace if k == "followup" and d["kind"] == "restart_dry"]
    assert fu and fu[0]["plug"] == "AMS1" and fu[0].get("shared") is True
    assert fu[0]["open"] is True


def test_the_engine_keeps_the_pause_across_a_restart():
    w = Cadence()
    eng = upstairs(w, x2d=300.0, heaters=[ams("AMS1"), ams("AMS2"), ams("AMS3")])
    w.run(eng, 75, [(T_ASK, ask(eng))])
    paused = [k for k, _ in answers(eng)[0]["paused"]]
    store = eng.to_store(60)
    assert all(store["plugs"][k]["shed_kind"] == "share" for k in paused)


# ---- the watch is unchanged when sharing is not used ------------------------------------------


@pytest.mark.parametrize("hot", [False, True])
def test_hot_panel_flag_reaches_the_model(hot):
    cfg, errs = validate_circuit({"id": "A", "shed_w": 1440, "restore_w": 1080, "hard_w": 1800,
                                  **({"hot_panel": True} if hot else {})})
    assert not errs
    assert cfg.hot_panel is hot
    assert cfg.trip_rating_w == (1800.0 * 0.95 if hot else 1800.0)


def test_hot_panel_trips_sooner_and_unset_is_today():
    w = Cadence()
    eng = Engine(w, timers=Timers())
    cold = eng.add_circuit(CircuitCfg(id="A", shed_w=1440, restore_w=1080, hard_w=1800),
                           [Plug("L", "other")])
    hot = eng.add_circuit(CircuitCfg(id="B", shed_w=1440, restore_w=1080, hard_w=1800,
                                     hot_panel=True), [Plug("M", "other")])
    cold.heat = hot.heat = 0.0
    assert cold.trip_in_s(1750) is None, "under the rating: never"
    assert hot.trip_in_s(1750) is not None, "a hot panel heats at 1,750 W (over 1,710 W)"
    assert hot.trip_in_s(1980) < cold.trip_in_s(1980)
    assert hot.landing_w == pytest.approx(1800 * 0.95 * 0.95)


def test_old_documents_and_other_values_mean_off():
    for v in (None, False, "yes", 1, "true"):
        cfg, _ = validate_circuit({"id": "A", "shed_w": 1440, "restore_w": 1080, "hard_w": 1800,
                                   "hot_panel": v})
        assert cfg.hot_panel is False, v


def test_status_is_as_before_when_sharing_and_hot_panel_are_unused():
    w = Cadence()
    eng = upstairs(w, heaters=[ams("AMS1")])
    w.run(eng, 30)
    st = eng.status(30)["circuits"][0]
    assert "sharing" not in st and "trip_rating_w" not in st
