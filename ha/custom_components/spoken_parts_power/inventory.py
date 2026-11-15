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

import re
from typing import Any

MODEL = "3RSP02064Z"
MANUFACTURER_HINT = "third reality"

CONFIG_SWITCH_SUFFIXES = ("_metering_only_mode", "_child_lock", "_led", "_indicator")
# ZHA unique ids end with <endpoint>-<cluster id>; the relay is the OnOff cluster (6).
ONOFF_RE = re.compile(r"-\d+-6$")
# Electrical measurement cluster (0x0B04 = 2820) for the watts sensor.
POWER_RE = re.compile(r"-\d+-2820(-active_power)?$")


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


def _meta(e: Any) -> dict[str, Any]:
    if isinstance(e, str):  # no registry facts: never trusted for the relay
        return {"entity_id": e, "platform": None}
    return dict(e)


def _is_config(m: dict[str, Any]) -> bool:
    obj = m["entity_id"].partition(".")[2]
    tk = str(m.get("translation_key") or "")
    uid = str(m.get("unique_id") or "")
    return (obj.endswith(CONFIG_SWITCH_SUFFIXES) or "metering" in tk or "metering" in uid
            or bool(m.get("entity_category")))


def classify(entities: list[Any]) -> dict[str, str | None]:
    """Map one device's entity registry entries to roles. Each entry is a dict with
    entity_id, platform, unique_id, translation_key, original_name, entity_category
    and device_class. Only ZHA entities count. The relay is the ZHA switch of the
    OnOff cluster with no entity category; a config switch (metering only mode,
    child lock) is never the relay. With no clear relay, the plug has none."""
    out: dict[str, str | None] = {
        "switch": None, "power": None, "voltage": None, "current": None, "rise": None,
        "drop": None, "metering_only": None, "power_on": None, "firmware": None,
    }
    relays: list[dict[str, Any]] = []
    for m in sorted((_meta(e) for e in entities), key=lambda m: m["entity_id"]):
        if m.get("platform") != "zha":
            continue
        eid = m["entity_id"]
        domain, _, obj = eid.partition(".")
        uid = str(m.get("unique_id") or "")
        if domain == "switch":
            if obj.endswith("_metering_only_mode") or "metering_only" in uid \
                    or "metering_only" in str(m.get("translation_key") or ""):
                out["metering_only"] = eid
            elif not _is_config(m):
                relays.append(m)
        elif domain == "sensor":
            dc = str(m.get("device_class") or "")
            if POWER_RE.search(uid) or dc == "power" or (
                    obj.endswith("_power") and not obj.endswith(("_power_factor", "_reactive_power"))):
                if dc in ("", "power") and out["power"] is None:
                    out["power"] = eid
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
    onoff = [m for m in relays if ONOFF_RE.search(str(m.get("unique_id") or ""))]
    if len(onoff) == 1:
        out["switch"] = onoff[0]["entity_id"]
    elif not onoff and len(relays) == 1:
        out["switch"] = relays[0]["entity_id"]
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
