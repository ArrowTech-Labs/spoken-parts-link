"""Random loads and report delays: the seven rules (DESIGN 6.1) always hold."""

import random

import pytest

from custom_components.spoken_parts_power.engine import Plug

from .world import World, engine, kinds, step

ROLES = ["ams_ht", "dryer", "printer", "printer", "printer", "other"]
STATES = ["idle", "starting", "printing", "printing", "paused", "finished"]


def random_world(seed):
    rng = random.Random(seed)
    w = World()
    plugs = []
    n = rng.randint(3, 7)
    for i in range(n):
        role = rng.choice(ROLES)
        key = f"{role[:2].upper()}{i}"
        points = [(0, rng.choice([0, 15, 60, 160, 300]))]
        for _ in range(rng.randint(1, 4)):
            points.append((rng.uniform(5, 500), rng.choice([0, 15, 160, 250, 400, 900, 1100])))
        points.sort()
        w.add(key, step(*points), report_delay=rng.uniform(0.1, 3.0), cmd_delay=rng.uniform(0.05, 1.0))
        plugs.append(Plug(key, role, peak_w=rng.choice([0, 180, 400])))
    eng = engine(w, plugs)
    script = []
    for p in plugs:
        if p.role == "printer":
            eng.feed_plug(p.key, rng.choice(STATES), rng.choice([None, 0, 500, 3000, 12000]), 0)
            when = rng.uniform(10, 400)
            st = rng.choice(STATES)
            val = rng.choice([None, 0, 800, 9000])
            script.append((when, lambda now, k=p.key, s=st, v=val: eng.feed_plug(k, s, v, now)))
    if rng.random() < 0.3:
        victim = rng.choice(plugs).key
        w.cmd_fail.add(victim)
    if rng.random() < 0.3:
        victim = rng.choice(plugs).key
        when = rng.uniform(20, 300)

        def lose(now, k=victim):
            w.silent.add(k)

        script.append((when, lose))
    return w, eng, script


@pytest.mark.parametrize("seed", range(40))
def test_seven_rules_hold(seed):
    w, eng, script = random_world(seed)
    w.run(eng, 900, script)
    trace = eng.trace
    offs = [(t, k) for t, k, a in eng.commands if a == "off"]
    ons = [(t, k) for t, k, a in eng.commands if a == "on"]

    # Rule 1: one shot per round. Every off goes out at a trip or at the sacrifice
    # decision of that round (or is the one retry of a failed command).
    shot_times = {round(t, 3) for t, k, d in trace if k in ("trip", "sacrifice_decision")}
    retried = {k for _, d in kinds(eng, "failed_open") for k in [d.get("plug")]} | w.cmd_fail
    for t, k in offs:
        assert round(t, 3) in shot_times or k in retried, (seed, t, k)

    # Rule 3: nothing is evaluated during a lockout.
    assert not [d for _, d in kinds(eng, "trip") if d["state"] == "LOCKOUT"], seed
    locks = []
    start = 0.0
    for t, k, d in trace:
        if k == "trip":
            start = t
        if k == "lockout_end":
            locks.append((start, t))
    for a, b in locks:
        assert not [1 for t, k, _ in trace if k == "trip" and a < t < b], (seed, a, b)

    # Rule 6: at most 3 rounds per event may cut a print. After that only free
    # loads (heaters, idle printers) can still be shed, and people are asked to act.
    last_round = 0
    for t, k, d in trace:
        if k == "trip":
            last_round = d["round"]
        if k == "shed" and any(tier == 3 for _key, _w, tier in d["plugs"]):
            assert last_round <= 3, (seed, t)

    # Fix A/B (Q-40, Q-41): a printer (idle or printing) is cut only at a sacrifice
    # decision that followed a re-read of every plug in the same round, on fresh
    # readings over the shed line.
    refreshed = None
    for t, k, d in trace:
        if k == "trip":
            refreshed = None
        if k == "refresh":
            refreshed = t
        if k == "shed" and any(tier >= 2 for _key, _w, tier in d["plugs"]):
            dec = [x for x in kinds(eng, "sacrifice_decision") if abs(x[0] - t) < 1e-9]
            assert dec and refreshed is not None and refreshed <= t, (seed, t)
            dd = dec[-1][1]
            assert dd["verified_sum"] > 1440, (seed, t, dd)

    # Rule 5: a print is cut only by a sacrifice decision on verified readings.
    sacrificed_at = {}
    for t, d in kinds(eng, "shed"):
        for key, _w, tier in d["plugs"]:
            if tier == 3:
                sacrificed_at[key] = t
                assert round(t, 3) in {round(x, 3) for x, _ in kinds(eng, "sacrifice_decision")}, seed

    # Rule 7: only what it turned off comes back; never a print it sacrificed.
    for t, k in ons:
        assert any(ot < t and ok == k for ot, ok in offs), (seed, k)
        assert k not in sacrificed_at or t < sacrificed_at[k], (seed, k)
    # One at a time: at least the settle time between two restores.
    times = sorted(t for t, _ in ons)
    for a, b in zip(times, times[1:]):
        assert b - a >= 90 - 1e-6, (seed, a, b)

    # No command for an `other` plug, ever.
    others = {p.key for p in eng.plugs.values() if p.role == "other"}
    assert not [k for _, k, _ in eng.commands if k in others], seed
