"""Paquete del cliente DFSha.

Los stubs generados por protoc (client/gen/*_pb2*.py) se importan entre sí con
nombres planos (`import datanode_pb2`), así que client/gen debe estar en sys.path.
"""
from __future__ import annotations

import sys
from pathlib import Path

_GEN_DIR = Path(__file__).resolve().parent / "gen"
if _GEN_DIR.is_dir() and str(_GEN_DIR) not in sys.path:
    sys.path.insert(0, str(_GEN_DIR))
