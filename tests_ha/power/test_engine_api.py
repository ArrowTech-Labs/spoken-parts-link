"""The engine's document, storage, journal and people-facing parts."""

from custom_components.spoken_parts_power.engine import Engine, Timers, validate_circuit

from .world import World, farm, followups, kinds, ons, p2_starts, step

CID = "11111111-1111-1111-1111-111111111111"


def doc(version=1, protect=True, plugs=None, **lines):
    c = {"id": CID, "name": "Circuit A", "protect": protect, "breaker_amps": 15, "volts": 120,
         "shed_w": 1440, "restore_w": 1080, "hard_w": 1800, "unmetered_w": 0, "sustain_s": 2.0}
    c.update(lines)
    return {"version": version, "site": 1, "circuits": [c], "plugs": plugs if plugs is not None else [
        {"id": "a", "ieee": "00:0d:6f:00:00:00:00:01", "circuit": CID, "role": "ams_ht",
         "label": "AMS HT 1", "may_turn_off": True, "rated_w": 180},
        {"id": "b", "ieee": "00:0d:6f:00:00:00:00:02", "circuit": CID, "role": "printer",
         "label": "H2D P1", "may_turn_off": True, "printer_serial": "01p00a"},
        {"id": "c", "ieee": "00:0d:6f:00:00:00:00:03", "circuit": None, "role": "other",
         "label": "Spare"},
    ]}


class Act:
    def __init__(self):
        self.switched, self.reads = [], []

    def switch(self, key, action, t):
        self.switched.append((t, key, action))

    def read(self, key, t):
        self.reads.append((t, key))
        return True


def test_guard_rails():
    ok, errs = validate_circuit(doc()["circuits"][0])
    assert ok and not errs
    for bad in ({"shed_w": 500}, {"restore_w": 1500}, {"hard_w": 2000}, {"hard_w": 1400},
                {"sustain_s": 30}, {"unmetered_w": 1500}):
        c = {**doc()["circuits"][0], **bad}
        assert validate_circuit(c)[0] is None, bad


def test_document_creates_a_live_circuit_that_reads_every_plug_first():
    act = Act()
    eng = Engine(act)
    res = eng.apply_config(doc(), 0)
    assert res["accepted"] and res["version"] == 1 and not res["errors"]
    c = eng.circuits[CID]
    assert c.state == "STARTING", "never fires from old data: it reads every plug first"
    assert sorted(k for _, k in act.reads) == ["00:0d:6f:00:00:00:00:01", "00:0d:6f:00:00:00:00:02"]
    assert set(c.plugs) == {"00:0d:6f:00:00:00:00:01", "00:0d:6f:00:00:00:00:02"}
    assert eng.plugs["00:0d:6f:00:00:00:00:02"].printer_serial == "01P00A"
    assert eng.plugs["00:0d:6f:00:00:00:00:03"].circuit is None
    eng.on_power("00:0d:6f:00:00:00:00:01", 100, 0.5)
    eng.on_power("00:0d:6f:00:00:00:00:02", 30, 0.6)
    eng.tick(0.75)
    assert c.state == "WATCH"


def test_bad_document_keeps_nothing_from_it():
    eng = Engine(Act())
    res = eng.apply_config(doc(shed_w=200), 0)
    assert res["errors"] and CID not in eng.circuits


def test_protect_off_then_on():
    act = Act()
    eng = Engine(act)
    eng.apply_config(doc(protect=False), 0)
    assert eng.circuits[CID].state == "OFF"
    eng.apply_config(doc(version=2), 1)
    assert eng.circuits[CID].state == "STARTING"


def test_config_waits_for_the_event_to_end():
    w = World()
    eng = farm(w)
    w.run(eng, 70, [p2_starts(eng)])
    c = eng.circuits["A"]
    assert c.event_open
    plugs = [{"ieee": k, "circuit": "A", "role": p.role} for k, p in eng.plugs.items()]
    d = {"version": 7, "circuits": [{"id": "A", "name": "A", "shed_w": 1400, "restore_w": 1000,
                                      "hard_w": 1800}], "plugs": plugs}
    res = eng.apply_config(d, 70)
    assert res["waiting"] == ["A"] and c.cfg.shed_w == 1440
    assert c.pending is not None


def test_journal_sequence_and_ack():
    eng = Engine(Act(), wall=lambda: "2026-11-15T10:00:00Z")
    eng.apply_config(doc(), 0)
    items = eng.journal.after(0)
    seqs = [e["seq"] for e in items]
    assert seqs == sorted(seqs) and seqs[0] == 1
    assert all(e["at"] == "2026-11-15T10:00:00Z" for e in items)
    last = seqs[-1]
    assert eng.journal.ack(last - 1) == 1
    assert [e["seq"] for e in eng.journal.after(0)] == [last]


def test_restart_keeps_held_plugs_and_fires_nothing_from_saved_data():
    w = World()
    eng = farm(w)
    w.run(eng, 100, [p2_starts(eng)])
    assert eng.plugs["HT1"].shed_kind == "heater"
    saved = eng.to_store(100)
    saved["config"] = None  # farm() sets up circuits directly; rebuild them the same way
    w2 = World()
    eng2 = farm(w2)
    eng2.from_store(saved, 0)
    assert eng2.plugs["HT1"].shed_kind == "heater"
    assert kinds(eng2, "engine_started")
    assert eng2.circuits["A"].state == "STARTING"
    eng2.tick(0.0)
    assert eng2.commands == []


def test_external_change_is_adopted_never_fought():
    w = World()
    eng = farm(w)
    w.run(eng, 100, [p2_starts(eng)])
    assert eng.plugs["HT1"].relay == "off"
    eng.on_relay("HT1", "on", 100.5)  # a person pressed the plug's button
    assert eng.plugs["HT1"].shed_kind is None
    assert kinds(eng, "external_change")
    w.relay["HT1"] = True
    w.run(eng, 1200, start=100.75)
    assert "HT1" not in [k for _, k in ons(eng)], "the engine never restores what a person took back"


def test_held_off_plug_reporting_power_late_is_only_failed_open_after_a_reread():
    w = World()
    eng = farm(w)
    w.run(eng, 80, [p2_starts(eng)])
    p = eng.plugs["HT1"]
    assert p.shed_kind == "heater" and p.relay == "off" and not p.zeroed
    reads = []
    eng.act = type("A", (), {"read": lambda s, k, t: reads.append(k) or True,
                             "switch": lambda s, k, a, t: None})()
    eng.on_power("HT1", 165, 81)  # one late report: not believed yet
    assert not p.failed_open and reads == ["HT1"]
    eng.on_power("HT1", 0, 81.4)  # the re-read says it is off
    assert not p.failed_open
    eng.on_power("HT1", 170, 90)
    eng.on_power("HT1", 170, 90.5)  # the re-read agrees: it really is drawing
    assert p.failed_open and followups(eng, "failed_open")


def test_restart_dry_follow_up():
    w = World()
    eng = farm(w, ht=165)
    w.loads["HT1"] = step((0, 165), (500, 3))  # after power returns it does not heat
    w.run(eng, 1000, [p2_starts(eng), (570, lambda n: eng.feed_plug("P2", "printing", 0, n))])
    assert "HT1" in [k for _, k in ons(eng)]
    rd = followups(eng, "restart_dry")
    assert rd and rd[0][1]["plug"] == "HT1"


def test_box_follows_the_event():
    w = World()
    eng = farm(w)
    c = eng.circuits["A"]
    boxes = []

    def look(now):
        b = eng.box(c, now)
        boxes.append(b["state"] if b else None)

    script = [p2_starts(eng), (570, lambda n: eng.feed_plug("P2", "printing", 0, n))]
    script += [(t, look) for t in (62.4, 63.0, 200, 665, 700, 800)]
    w.run(eng, 1000, script)
    look(c.closed_at + 1)
    assert boxes[0] == "checking" and boxes[1] == "checking"
    assert boxes[2] == "waiting_heat"
    assert "hold" in boxes[3:5]
    assert "restoring" in boxes
    assert boxes[-1] == "all_clear"


def test_status_shape():
    eng = Engine(Act())
    eng.apply_config(doc(), 0)
    eng.on_power("00:0d:6f:00:00:00:00:01", 120, 1)
    st = eng.status(2)
    c = st["circuits"][0]
    assert c["id"] == CID and c["lines"]["target_w"] == 1368 and c["sum_w"] == 120
    assert st["plugs"]["00:0d:6f:00:00:00:00:01"]["w"] == 120
    assert st["config_version"] == 1


def test_heartbeat_reads_a_quiet_plug_and_marks_a_silent_one():
    act = Act()
    eng = Engine(act, timers=Timers())
    eng.apply_config(doc(), 0)
    key = "00:0d:6f:00:00:00:00:01"
    eng.on_power(key, 100, 0.5)
    eng.on_power("00:0d:6f:00:00:00:00:02", 30, 0.5)
    act.reads.clear()
    for i in range(1, 400):
        eng.tick(i * 0.25)
    assert (key in [k for _, k in act.reads]), "read after 30 s without a report"
    assert eng.plugs[key].silent, "no answer for 90 s: not reachable"
    eng.on_power(key, 101, 100)
    assert not eng.plugs[key].silent
