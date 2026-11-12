"""The fake 4x6 ZPL driver from test_registry, importable from the glue tests."""

import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from test_registry import Fake4x6  # noqa: E402

__all__ = ["Fake4x6"]
