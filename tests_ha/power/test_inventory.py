from custom_components.spoken_parts_power.inventory import (
    checks,
    classify,
    ieee_of,
    is_supported,
    previous_option,
)

BASE = "3d_printer_room_plug_for_x2d_p_0002"
ENTS = [f"switch.{BASE}", f"sensor.{BASE}_power", f"sensor.{BASE}_voltage", f"sensor.{BASE}_current",
        f"number.{BASE}_power_rise_threshold", f"number.{BASE}_power_drop_threshold",
        f"switch.{BASE}_metering_only_mode", f"select.{BASE}_power_on_behavior",
        f"update.{BASE}_firmware", f"binary_sensor.{BASE}_opening"]


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
    assert checks(classify([f"switch.{BASE}"]), {}, True)["thresholds_1w"] is None


def test_only_thirdreality_gen3():
    assert is_supported("3RSP02064Z", "Third Reality, Inc")
    assert not is_supported("TS011F", "_TZ3000_xyz")
    assert not is_supported("3RSP02028BZ", "Third Reality, Inc")


def test_ieee_and_option():
    assert ieee_of({("zha", "00:0D:6F:AA")}) == "00:0d:6f:aa"
    assert ieee_of({("mqtt", "x")}) is None
    assert previous_option(["Off", "On", "Toggle", "PreviousValue"]) == "PreviousValue"
    assert previous_option(["Off", "On"]) is None
