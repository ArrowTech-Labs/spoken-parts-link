import pytest

from custom_components.spoken_parts_printers.drivers import registry
from custom_components.spoken_parts_printers.drivers.base import (
    Capabilities,
    Driver,
    Media,
    PrintJob,
    SppParams,
)


class Fake4x6(Driver):
    """A pretend 4x6 shipping printer that takes ZPL, like a later driver would."""

    id = "acme.ship4x6"
    vendor = "Acme"
    model = "Ship 4x6"
    capabilities = Capabilities(
        dpi=203,
        print_width_dots=812,
        media=(Media("102x152", "shipping_4x6", 102, 152, gap_mm=3),),
        default_media="102x152",
        transports=("fake",),
        formats=("zpl",),
    )

    def encode(self, job: PrintJob) -> bytes:
        return b"".join(p.data for p in job.pages) * job.copies


@pytest.fixture
def fake_driver():
    d = registry.register(Fake4x6())
    yield d
    registry.unregister(d.id)


def test_builtin_drivers_registered():
    ids = [d.id for d in registry.all()]
    assert "nelko.p21" in ids and "nelko.pm220" in ids
    assert registry.get("nelko.p21").model == "P21"
    assert registry.get("nope.nope") is None


def test_second_driver_registers_and_shows_in_choices(fake_driver):
    assert registry.get("acme.ship4x6") is fake_driver
    assert fake_driver in registry.all()
    choices = registry.choices()
    assert choices["acme.ship4x6"] == "Acme Ship 4x6"
    assert choices["nelko.p21"] == "Nelko P21"


def test_4x6_capabilities_differ(fake_driver):
    p21 = registry.get("nelko.p21").capabilities
    c = fake_driver.capabilities
    assert c.formats == ("zpl",) and p21.formats == ("raster",)
    assert c.print_width_dots == 812 != p21.print_width_dots
    assert c.media[0].kind == "shipping_4x6"
    assert fake_driver.describe()["capabilities"]["formats"] == ["zpl"]


def test_duplicate_and_invalid_registration(fake_driver):
    with pytest.raises(ValueError):
        registry.register(Fake4x6())
    registry.register(Fake4x6(), replace=True)

    class NoParams(Fake4x6):
        id = "acme.noparams"
        capabilities = Capabilities(
            dpi=203, print_width_dots=8, media=(Media("10x10", "die_cut", 10, 10),),
            default_media="10x10", transports=("spp",),
        )

    with pytest.raises(ValueError):
        registry.register(NoParams())

    class WithParams(NoParams):
        id = "acme.withparams"
        spp = SppParams()

    registry.register(WithParams())
    registry.unregister("acme.withparams")


def test_bad_capabilities_rejected():
    with pytest.raises(ValueError):
        Capabilities(dpi=203, print_width_dots=8, media=(Media("1x1", "die_cut", 1, 1),),
                     default_media="2x2")
    with pytest.raises(ValueError):
        Capabilities(dpi=203, print_width_dots=8, media=(Media("1x1", "die_cut", 1, 1),),
                     default_media="1x1", transports=("carrier_pigeon",))
