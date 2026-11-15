from custom_components.spoken_parts_power.inventory import (
    checks,
    classify,
    ieee_of,
    is_supported,
    previous_option,
)

BASE = "3d_printer_room_plug_for_x2d_p_0002"
IEEE = "28:2c:02:bf:ff:e8:5b:3a"


def zha(eid, uid, cat=None, tk=None, dc=None, platform="zha"):
    return {"entity_id": eid, "platform": platform, "unique_id": f"{IEEE}-{uid}",
            "entity_category": cat, "translation_key": tk, "device_class": dc}


ENTS = [zha(f"switch.{BASE}", "1-6"), zha(f"sensor.{BASE}_power", "1-2820", dc="power"),
        zha(f"sensor.{BASE}_voltage", "1-2820-rms_voltage", dc="voltage"),
        zha(f"sensor.{BASE}_current", "1-2820-rms_current", dc="current"),
        zha(f"number.{BASE}_power_rise_threshold", "1-65535-power_rise_threshold", cat="config"),
        zha(f"number.{BASE}_power_drop_threshold", "1-65535-power_drop_threshold", cat="config"),
        zha(f"switch.{BASE}_metering_only_mode", "1-65535-metering_only_mode", cat="config",
            tk="metering_only_mode"),
        zha(f"select.{BASE}_power_on_behavior", "1-6-start_up_on_off", cat="config"),
        zha(f"update.{BASE}_firmware", "1-25-firmware_update", cat="config"),
        zha(f"binary_sensor.{BASE}_opening", "1-6-opening")]


def test_classify_marcs_plug():
    r = classify(ENTS)
    assert r["switch"] == f"switch.{BASE}"
    assert r["power"] == f"sensor.{BASE}_power"
    assert r["metering_only"] == f"switch.{BASE}_metering_only_mode"
    assert r["rise"].endswith("_rise_threshold") and r["drop"].endswith("_drop_threshold")
    assert r["power_on"] == f"select.{BASE}_power_on_behavior"
    assert r["firmware"] == f"update.{BASE}_firmware"


def test_checks():
    r = classify(ENTS)
    states = {r["rise"]: "3200", r["drop"]: "3200", r["metering_only"]: "off",
              r["power_on"]: "On", r["firmware"]: "off"}
    c = checks(r, states, True)
    assert c["thresholds_1w"] is False and c["metering_only_off"] is True
    assert c["power_on_previous"] is False and c["firmware_current"] is True
    states.update({r["rise"]: "1", r["drop"]: "1.0", r["power_on"]: "PreviousValue"})
    c = checks(r, states, True)
    assert c["thresholds_1w"] is True and c["power_on_previous"] is True
    assert checks(classify([ENTS[0]]), {}, True)["thresholds_1w"] is None


def test_10_relay_is_the_zha_onoff_switch_never_metering_only_or_another_platform():
    # A template or other integration's switch on the same device is never the relay.
    other = zha("switch.a", "x", platform="template")
    assert classify(ENTS + [other])["switch"] == f"switch.{BASE}"
    # Only the metering only switch left: no relay at all (never that one).
    no_main = [e for e in ENTS if e["entity_id"] != f"switch.{BASE}"]
    assert classify(no_main)["switch"] is None
    assert classify(no_main)["metering_only"] == f"switch.{BASE}_metering_only_mode"
    # A short-named config switch does not win over the OnOff cluster switch.
    lock = zha("switch.a", "1-0-child_lock", cat="config")
    assert classify(ENTS + [lock])["switch"] == f"switch.{BASE}"
    # Bare entity ids (no registry facts) are never trusted.
    assert classify([f"switch.{BASE}", f"sensor.{BASE}_power"]) == {
        k: None for k in classify([])}
    # Two OnOff switches on one device: ambiguous, so none.
    two = ENTS + [zha("switch.b", "2-6")]
    assert classify(two)["switch"] is None


def test_only_thirdreality_gen3():
    assert is_supported("3RSP02064Z", "Third Reality, Inc")
    assert not is_supported("TS011F", "_TZ3000_xyz")
    assert not is_supported("3RSP02028BZ", "Third Reality, Inc")


def test_ieee_and_option():
    assert ieee_of({("zha", "00:0D:6F:AA")}) == "00:0d:6f:aa"
    assert ieee_of({("mqtt", "x")}) is None
    assert previous_option(["Off", "On", "Toggle", "PreviousValue"]) == "PreviousValue"
    assert previous_option(["Off", "On"]) is None
