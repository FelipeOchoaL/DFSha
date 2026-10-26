# Cliente gRPC de DataNode: WriteBlock y ReadBlock en streaming usando los stubs de client/gen/.
"""Cliente gRPC de DataNode: escritura y lectura de bloques por streaming (US-32)."""
from __future__ import annotations

import hashlib
import logging
from dataclasses import dataclass
from typing import Iterator, Mapping, Sequence

import grpc

import datanode_pb2 as pb  # client/gen está en sys.path (ver client/__init__.py)
import datanode_pb2_grpc as pb_grpc

from client.core.errors import (
    BlockCorruptedError,
    BlockNotFoundError,
    DataNodeError,
    DataNodeUnavailableError,
)

log = logging.getLogger(__name__)

CHUNK_SIZE = 64 * 1024

# endpoint = {"id": ..., "host": ..., "port": ..., "rack": ...} (como lo devuelve allocate)
Endpoint = Mapping[str, object]


@dataclass(frozen=True)
class WriteResult:
    block_id: str
    checksum_sha256: str
    bytes_written: int
    replica_ids: tuple[str, ...]


class DataNodeClient:
    def __init__(self, timeout: float = 60.0, ca_cert: str | None = None):
        self._timeout = timeout
        self._credentials = None
        if ca_cert:
            with open(ca_cert, "rb") as f:
                self._credentials = grpc.ssl_channel_credentials(root_certificates=f.read())

    # ---------- API pública ----------

    def write_block(self, endpoint: Endpoint, block_id: str, data: bytes,
                    checksum: str, pipeline: Sequence[Endpoint] = ()) -> WriteResult:
        """Envía el bloque al primer nodo; `pipeline` son los nodos que siguen."""
        node_id = str(endpoint.get("id", "?"))
        try:
            with self._channel(endpoint) as channel:
                stub = pb_grpc.DataNodeStub(channel)
                resp = stub.WriteBlock(
                    self._write_requests(block_id, data, checksum, pipeline),
                    timeout=self._timeout,
                )
        except grpc.RpcError as exc:
            raise self._translate(exc, node_id, block_id) from None

        if resp.checksum_sha256 != checksum or resp.bytes_written != len(data):
            raise BlockCorruptedError(
                f"El nodo {node_id} confirmó datos distintos a los enviados (bloque {block_id})",
                node_id, block_id)
        return WriteResult(resp.block_id, resp.checksum_sha256, resp.bytes_written,
                           tuple(resp.replica_ids))

    def read_block(self, endpoint: Endpoint, block_id: str, offset: int = 0, length: int = 0) -> bytes:
        """Lee un bloque. Verifica checksum si se lee completo (offset=0, length=0)."""
        node_id = str(endpoint.get("id", "?"))
        header = None
        buf = bytearray()
        try:
            with self._channel(endpoint) as channel:
                stub = pb_grpc.DataNodeStub(channel)
                request = pb.ReadBlockRequest(block_id=block_id, offset=offset, length=length)
                for msg in stub.ReadBlock(request, timeout=self._timeout):
                    kind = msg.WhichOneof("payload")
                    if kind == "header":
                        header = msg.header
                    elif kind == "chunk":
                        if header is None:
                            raise DataNodeError("El nodo envió datos antes del header", node_id, block_id)
                        buf += msg.chunk
        except grpc.RpcError as exc:
            raise self._translate(exc, node_id, block_id) from None

        if header is None:
            raise DataNodeError("El nodo no envió header", node_id, block_id)

        data = bytes(buf)
        if offset == 0 and length == 0:
            if len(data) != header.size or hashlib.sha256(data).hexdigest() != header.checksum_sha256:
                raise BlockCorruptedError(
                    f"Checksum inválido leyendo el bloque {block_id} del nodo {node_id}",
                    node_id, block_id)
        return data

    # ---------- internos ----------

    def _channel(self, endpoint: Endpoint) -> grpc.Channel:
        target = f"{endpoint['host']}:{endpoint['port']}"
        if self._credentials is not None:
            return grpc.secure_channel(target, self._credentials)
        return grpc.insecure_channel(target)

    @staticmethod
    def _to_pb(endpoint: Endpoint) -> pb.DataNodeEndpoint:
        return pb.DataNodeEndpoint(
            id=str(endpoint.get("id", "")),
            host=str(endpoint["host"]),
            port=int(endpoint["port"]),
            rack=str(endpoint.get("rack", "")),
        )

    @classmethod
    def _write_requests(cls, block_id: str, data: bytes, checksum: str,
                        pipeline: Sequence[Endpoint]) -> Iterator[pb.WriteBlockRequest]:
        header = pb.WriteBlockHeader(
            block_id=block_id,
            size=len(data),
            checksum_sha256=checksum,
            pipeline=[cls._to_pb(e) for e in pipeline],
        )
        yield pb.WriteBlockRequest(header=header)
        view = memoryview(data)
        for i in range(0, len(data), CHUNK_SIZE):
            yield pb.WriteBlockRequest(chunk=bytes(view[i:i + CHUNK_SIZE]))

    @staticmethod
    def _translate(exc: grpc.RpcError, node_id: str, block_id: str) -> DataNodeError:
        # El texto original de gRPC suele incluir host:puerto: solo va al log de debug.
        log.debug("gRPC error con nodo %s: %s", node_id, exc)
        code = exc.code()
        details = exc.details() or ""
        if code == grpc.StatusCode.NOT_FOUND:
            return BlockNotFoundError(f"El nodo {node_id} no tiene el bloque {block_id}", node_id, block_id)
        if code == grpc.StatusCode.DATA_LOSS:
            return BlockCorruptedError(f"Bloque {block_id} corrupto en el nodo {node_id}", node_id, block_id)
        if code in (grpc.StatusCode.UNAVAILABLE, grpc.StatusCode.DEADLINE_EXCEEDED):
            return DataNodeUnavailableError(f"El nodo {node_id} no responde", node_id, block_id)
        return DataNodeError(f"Error del nodo {node_id} ({code.name}): {details}", node_id, block_id)