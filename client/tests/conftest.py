"""Hace importable `client` al correr pytest desde cualquier carpeta."""
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))

import client  # noqa: E402,F401  (añade client/gen al sys.path)
