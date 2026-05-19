#!/usr/bin/env python3
from __future__ import annotations

import sys
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
AUTOREG = ROOT / "services" / "autoreg"
if str(AUTOREG) not in sys.path:
    sys.path.insert(0, str(AUTOREG))

from scripts.find_cdp_endpoint import main  # noqa: E402


if __name__ == "__main__":
    raise SystemExit(main())
