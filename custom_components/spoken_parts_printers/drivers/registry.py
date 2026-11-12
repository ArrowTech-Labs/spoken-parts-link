"""Driver registry. A new printer model is a module that calls register().

Built in drivers register themselves when the drivers package is imported
(see drivers/__init__.py). Nothing here imports Home Assistant.
"""

from __future__ import annotations

import builtins

from .base import Driver

_DRIVERS: dict[str, Driver] = {}


def register(driver: Driver, *, replace: bool = False) -> Driver:
    """Add a driver instance. Returns it so a module can write X = register(X())."""
    if not driver.id or "." not in driver.id:
        raise ValueError(f"driver id must look like vendor.model, got {driver.id!r}")
    if driver.id in _DRIVERS and not replace and _DRIVERS[driver.id] is not driver:
        raise ValueError(f"driver {driver.id} is already registered")
    caps = driver.capabilities
    for t in caps.transports:
        if t in ("spp", "ble") and driver.transport_params(t) is None:
            raise ValueError(f"driver {driver.id} offers {t} but has no {t} parameters")
    _DRIVERS[driver.id] = driver
    return driver


def unregister(driver_id: str) -> None:
    """Remove a driver (for tests)."""
    _DRIVERS.pop(driver_id, None)


def get(driver_id: str) -> Driver | None:
    return _DRIVERS.get(driver_id)


def all() -> builtins.list[Driver]:  # noqa: A001 (the name is the public API)
    return [_DRIVERS[k] for k in sorted(_DRIVERS)]


def choices() -> dict[str, str]:
    """Driver id to a human label, for the config flow."""
    return {d.id: f"{d.vendor} {d.model}" for d in all()}
