"""Partición de archivos en bloques de tamaño fijo (US-31).

Nunca mantiene más de un bloque en memoria (RNF4).
"""
from __future__ import annotations

import hashlib
import math
import os
from pathlib import Path
from typing import Iterator, NamedTuple


class Chunk(NamedTuple):
    index: int
    offset: int
    data: bytes
    sha256: str


def iter_chunks(path: str | os.PathLike, block_size: int) -> Iterator[Chunk]:
    """Devuelve (índice, offset, bytes, sha256) por cada bloque.

    El último bloque puede ser menor. Un archivo vacío no produce bloques.
    """
    if block_size <= 0:
        raise ValueError("block_size debe ser positivo")
    with open(path, "rb") as f:
        index = 0
        offset = 0
        while True:
            data = f.read(block_size)
            if not data:
                break
            yield Chunk(index, offset, data, hashlib.sha256(data).hexdigest())
            index += 1
            offset += len(data)


def count_blocks(path: str | os.PathLike, block_size: int) -> int:
    """Número de bloques sin leer el archivo (para mostrar 'bloque i de N')."""
    size = Path(path).stat().st_size
    return math.ceil(size / block_size) if size else 0