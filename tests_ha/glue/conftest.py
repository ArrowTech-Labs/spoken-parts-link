"""Home Assistant glue tests. Skipped unless pytest-homeassistant-custom-component
is installed. Run them on their own: pytest tests_ha/glue"""

import pytest

pytest.importorskip("pytest_homeassistant_custom_component")


@pytest.fixture(autouse=True)
def auto_enable(enable_custom_integrations, enable_bluetooth):
    """Load custom_components/ and a mocked Bluetooth stack in every test."""
    yield


@pytest.fixture
def expected_lingering_timers() -> bool:
    """habluetooth's scanner (started by the enable_bluetooth fixture, not by
    this integration) leaves its device expiry timer behind on teardown."""
    return True
