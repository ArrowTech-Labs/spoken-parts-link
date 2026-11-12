"""Tests for custom_components/spoken_parts_printers.

The tests at this level need only pytest: they cover the printer core
(drivers, transports, runtime), which does not import Home Assistant.
The tests in glue/ need pytest-homeassistant-custom-component and are
skipped when it is not installed.
"""

import asyncio
import sys
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parent.parent
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))


def run(coro):
    """Run a coroutine in a fresh event loop (no asyncio plugin needed)."""
    return asyncio.run(coro)


@pytest.fixture
def p21():
    from custom_components.spoken_parts_printers.drivers import registry

    return registry.get("nelko.p21")


@pytest.fixture
def pm220():
    from custom_components.spoken_parts_printers.drivers import registry

    return registry.get("nelko.pm220")
