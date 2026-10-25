"""put <local> <remoto> (US-34 + reintento por bloque US-53).

Por cada bloque: allocate -> WriteBlock al primer DataNode del pipeline (él replica
al resto) -> commit. Al final finalize. Si un bloque falla se pide otro allocate
para ese mismo índice (nuevo pipeline) hasta MAX_RETRIES veces.
"""
from __future__ import annotations

import contextlib
import sys
from pathlib import Path

from client.commands._common import datanode, namenode, remember_namenode
from client.core.chunker import Chunk, count_blocks, iter_chunks
from client.core.errors import DataNodeError, DfshaError, NameNodeError
from client.core.namenode_api import NameNodeAPI, pick

MAX_RETRIES = 3


def run(args, config, session) -> int:
    local = Path(args.local)
    if not local.is_file():
        raise DfshaError(f"No existe el archivo local: {local}")
    remote = session.resolve(args.remote)

    api = namenode(config, session)
    dn = datanode(config)
    block_size = config.block_size_bytes
    total = count_blocks(local, block_size)
    if total == 0:
        raise DfshaError("El archivo está vacío; DFSha v1 no almacena archivos de 0 bytes")

    started = False
    try:
        for chunk in iter_chunks(local, block_size):
            print(f"bloque {chunk.index + 1}/{total} ({len(chunk.data)} B)...", end=" ", flush=True)
            result = upload_block(api, dn, remote, chunk)
            started = True
            print(f"ok -> réplicas {', '.join(result) or '?'}")
        api.finalize(remote)
    except DfshaError:
        if started:
            # No dejar un archivo UNDER_CONSTRUCTION huérfano.
            with contextlib.suppress(DfshaError):
                api.rm(remote)
        raise

    remember_namenode(api, session)
    print(f"{local.name} -> {remote} ({total} bloque{'s' if total != 1 else ''})")
    return 0


def upload_block(api: NameNodeAPI, dn, remote: str, chunk: Chunk) -> tuple[str, ...]:
    """Sube un bloque con reintentos. Devuelve los ids de las réplicas confirmadas."""
    last: Exception | None = None
    for attempt in range(1, MAX_RETRIES + 1):
        alloc = api.allocate(remote, chunk.index, chunk.offset, len(chunk.data))
        block_id = pick(alloc, "blockId", "block_id")
        pipeline = pick(alloc, "pipeline", "datanodes", "replicas", default=[])
        if not block_id or not pipeline:
            raise NameNodeError("allocate no devolvió blockId o pipeline")
        try:
            result = dn.write_block(pipeline[0], block_id, chunk.data, chunk.sha256,
                                    pipeline=pipeline[1:])
            api.commit(remote, block_id, len(chunk.data), chunk.sha256,
                       replica_ids=result.replica_ids)
            return result.replica_ids
        except (DataNodeError, NameNodeError) as exc:
            last = exc
            if isinstance(exc, NameNodeError) and exc.status in (401, 403, 404):
                raise  # no tiene sentido reintentar
            print(f"\n  intento {attempt}/{MAX_RETRIES} falló: {exc}", file=sys.stderr)
    raise DfshaError(f"No se pudo escribir el bloque {chunk.index} tras {MAX_RETRIES} intentos: {last}")
