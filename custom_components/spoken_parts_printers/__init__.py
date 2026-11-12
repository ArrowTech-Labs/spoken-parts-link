"""Spoken Parts Printers: print labels from Spoken Parts on Bluetooth label printers.

The Home Assistant glue lives in integration.py. The printer core (drivers,
transports, printer runtime) does not import Home Assistant, so it can be unit
tested without it. That is why the import below is guarded.
"""

from __future__ import annotations

try:
    import homeassistant.core  # noqa: F401
except ModuleNotFoundError as err:  # the core is being used without Home Assistant
    if not (err.name or "").startswith("homeassistant"):
        raise
else:
    from .integration import (  # noqa: F401
        CONFIG_SCHEMA,
        async_setup,
        async_setup_entry,
        async_unload_entry,
    )
