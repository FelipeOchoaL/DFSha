"""get <remoto> <local> (US-35 + lectura paralela y reintento por réplica US-37).

Pide las ubicaciones al NameNode, descarga los bloques en paralelo (cada uno
prueba sus réplicas en orden) y escribe cada bloque en su offset dentro de un
archivo temporal que se renombra al final.
"""
from __future__ import annotations

import os
import sys
from concurrent.futures import ThreadPoolExecutor, as_completed
from pathlib import Path

from client.commands._common import datanode, namenode, remember_namenode
from client.core.errors import DataNodeError, DfshaError
from client.core.namenode_api import pick


def run(args, config, session) -> int:
    remote = session.resolve(args.remote)
    local = Path(args.local)
    if local.is_dir():
        local = local / Path(remote).name

    api = namenode(config, session)
    dn = datanode(config)

    blocks = _blocks(api.locations(remote))
    if not blocks:
        raise DfshaError(f"{remote} no tiene bloques")

    tmp = local.with_name(local.name + ".part")
    total_size = sum(b["size"] for b in blocks)
    with open(tmp, "wb") as f:
        if total_size:
            f.truncate(total_size)

    try:
        with ThreadPoolExecutor(max_workers=config.max_threads) as pool:
            futures = {pool.submit(_fetch, dn, b, tmp): b for b in blocks}
            for fut in as_completed(futures):
                b = futures[fut]
                node = fut.result()  # relanza DfshaError si todas las réplicas fallaron
                print(f"bloque {b['index'] + 1}/{len(blocks)} ok (nodo {node})")
        os.replace(tmp, local)
    except BaseException:
        try:
            tmp.unlink()
        except OSError:
            pass
        raise

    remember_namenode(api, session)
    print(f"{remote} -> {local} ({total_size} B, {len(blocks)} bloques)")
    return 0


def _fetch(dn, block: dict, tmp: Path) -> str:
    """Descarga un bloque probando sus réplicas en orden. Devuelve el id del nodo usado."""
    errors: list[str] = []
    for ep in block["locations"]:
        try:
            data = dn.read_block(ep, block["block_id"])
        except DataNodeError as exc:
            errors.append(str(exc))
            continue
        if block["size"] and len(data) != block["size"]:
            errors.append(f"nodo {ep.get('id')}: tamaño {len(data)} != {block['size']}")
            continue
        if block["checksum"] and _sha256(data) != block["checksum"]:
            errors.append(f"nodo {ep.get('id')}: checksum distinto al del NameNode")
            continue
        with open(tmp, "r+b") as f:
            f.seek(block["offset"])
            f.write(data)
        return str(ep.get("id", "?"))
    detail = "; ".join(errors) or "sin réplicas"
    print(f"bloque {block['index']}: todas las réplicas fallaron", file=sys.stderr)
    raise DfshaError(f"No se pudo leer el bloque {block['index']} ({block['block_id']}): {detail}")


def _sha256(data: bytes) -> str:
    import hashlib
    return hashlib.sha256(data).hexdigest()


def _blocks(payload: dict) -> list[dict]:
    """Normaliza la respuesta de /files/locations a una lista ordenada por índice."""
    raw = pick(payload, "blocks", "locations", default=[])
    blocks = []
    offset = 0
    for i, b in enumerate(raw):
        size = int(pick(b, "size", "length", default=0))
        idx = int(pick(b, "index", "blockIndex", "block_index", default=i))
        blocks.append({
            "index": idx,
            "block_id": pick(b, "blockId", "block_id"),
            "size": size,
            "offset": int(pick(b, "offset", default=offset)),
            "checksum": pick(b, "checksumSha256", "checksum_sha256", "checksum", default=""),
            "locations": pick(b, "locations", "datanodes", "replicas", default=[]),
        })
        offset += size
    blocks.sort(key=lambda b: b["index"])
    return blocks
