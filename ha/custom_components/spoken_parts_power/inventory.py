"""Which entities of a ZHA plug are which, and the per-plug checks (DESIGN 5.3, 5.5).

No Home Assistant imports: the glue passes plain dicts in, so this is tested
without Home Assistant. Entities are matched within one device (same device id),
by domain and by the end of the entity id, which is how ZHA names them for the
THIRDREALITY Smart Plug Gen3 on Home Assistant 2026.11:

  switch.<base>                          the relay
  sensor.<base>_power                    watts
  sensor.<base>_voltage, _current
  number.<base>_power_rise_threshold     W (wanted: 1)
  number.<base>_power_drop_threshold     W (wanted: 1)
  switch.<base>_metering_only_mode       (wanted: off)
  select.<base>_power_on_behavior        (wanted: PreviousValue)
  update.<base>_firmware
"""

from __future__ import annotations

from typing import Any

MODEL = "3RSP02064Z"
MANUFACTURER_HINT = "third reality"

CONFIG_SWITCH_SUFFIXES = ("_metering_only_mode", "_child_lock", "_led", "_indicator")


def is_supported(model: str | None, manufacturer: str | None) -> bool:
    """Only THIRDREALITY Smart Plug Gen3 plugs may be switched by the engine."""
    if (model or "").strip().upper() != MODEL:
        return False
    return MANUFACTURER_HINT in (manufacturer or "").lower().replace(",", "") or not manufacturer


def ieee_of(identifiers: list[tuple[str, str]] | set) -> str | None:
    for domain, value in identifiers:
        if domain == "zha" and value:
            return str(value).strip().lower()
    return None


def classify(entity_ids: list[str]) -> dict[str, str | None]:
    """Map one device's entity ids to roles."""
    out: dict[str, str | None] = {
        "switch": None, "power": None, "voltage": None, "current": None, "rise": None,
        "drop": None, "metering_only": None, "power_on": None, "firmware": None,
    }
    switches = []
    for eid in sorted(entity_ids):
        domain, _, obj = eid.partition(".")
        if domain == "switch":
            if obj.endswith("_metering_only_mode"):
                out["metering_only"] = eid
            elif not obj.endswith(CONFIG_SWITCH_SUFFIXES):
                switches.append(eid)
        elif domain == "sensor":
            if obj.endswith("_power") and not obj.endswith(("_power_factor", "_reactive_power")):
                out["power"] = out["power"] or eid
            elif obj.endswith("_voltage"):
                out["voltage"] = eid
            elif obj.endswith("_current"):
                out["current"] = eid
        elif domain == "number":
            if obj.endswith("_power_rise_threshold"):
                out["rise"] = eid
            elif obj.endswith("_power_drop_threshold"):
                out["drop"] = eid
        elif domain == "select" and obj.endswith("_power_on_behavior"):
            out["power_on"] = eid
        elif domain == "update" and out["firmware"] is None:
            out["firmware"] = eid
    if switches:
        out["switch"] = min(switches, key=len)  # the relay has the shortest name
    return out


def _num(v: Any) -> float | None:
    try:
        return float(v)
    except (TypeError, ValueError):
        return None


def checks(ents: dict[str, str | None], states: dict[str, Any], model_ok: bool) -> dict[str, Any]:
    """True, False, or None when Home Assistant does not show the setting."""
    def st(role: str) -> Any:
        eid = ents.get(role)
        return states.get(eid) if eid else None

    rise, drop = _num(st("rise")), _num(st("drop"))
    thresholds = None if rise is None and drop is None else (
        rise is not None and drop is not None and rise <= 1 and drop <= 1)
    mo = st("metering_only")
    po = st("power_on")
    fw = st("firmware")
    return {
        "model_ok": model_ok,
        "has_switch_and_power": bool(ents.get("switch") and ents.get("power")),
        "thresholds_1w": thresholds,
        "rise_w": rise,
        "drop_w": drop,
        "metering_only_off": None if mo in (None, "unavailable", "unknown") else mo == "off",
        "power_on_previous": None if po in (None, "unavailable", "unknown")
        else "previous" in str(po).lower(),
        "firmware_current": None if fw in (None, "unavailable", "unknown") else fw == "off",
    }


def previous_option(options: list[str] | None) -> str | None:
    """The power-on option that means "as before" (ZHA: PreviousValue)."""
    for o in options or []:
        if "previous" in str(o).lower():
            return o
    return None
