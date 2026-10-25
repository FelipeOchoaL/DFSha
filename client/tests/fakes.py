"""Servidores falsos que respetan los contratos, para probar el cliente sin Go.

- FakeDataNode: gRPC según proto/datanode.proto (WriteBlock/ReadBlock/DeleteBlock).
  Reenvía al siguiente del pipeline si se le pasa un registro de nodos.
- FakeNameNode: HTTP según las rutas de namenode_api.py. Metadatos en memoria y
  colocación round-robin sobre los DataNodes registrados.
"""
from __future__ import annotations

import hashlib
import json
import threading
from concurrent import futures
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs, urlparse

import grpc

import datanode_pb2 as pb
import datanode_pb2_grpc as pb_grpc


# ---------------------------------------------------------------- DataNode

class FakeDataNode(pb_grpc.DataNodeServicer):
    def __init__(self, node_id: str, rack: str = "A", registry: dict | None = None,
                 fail_reads: bool = False):
        self.id = node_id
        self.rack = rack
        self.blocks: dict[str, bytes] = {}
        self.registry = registry if registry is not None else {}
        self.fail_reads = fail_reads
        self.lie_checksum = False  # devuelve un checksum falso en el header de ReadBlock
        self.alive = False
        self.port = 0
        self._server = None

    # -- ciclo de vida
    def start(self) -> "FakeDataNode":
        self._server = grpc.server(futures.ThreadPoolExecutor(max_workers=8))
        pb_grpc.add_DataNodeServicer_to_server(self, self._server)
        self.port = self._server.add_insecure_port("127.0.0.1:0")
        self._server.start()
        self.alive = True
        self.registry[self.id] = self
        return self

    def stop(self):
        if self._server:
            self._server.stop(0)
            self._server = None
        self.alive = False
        self.registry.pop(self.id, None)

    @property
    def endpoint(self) -> dict:
        return {"id": self.id, "host": "127.0.0.1", "port": self.port, "rack": self.rack}

    # -- RPCs
    def WriteBlock(self, request_iterator, context):
        first = next(request_iterator)
        if first.WhichOneof("payload") != "header":
            context.abort(grpc.StatusCode.INVALID_ARGUMENT, "first message must be header")
        h = first.header
        buf = bytearray()
        for msg in request_iterator:
            buf += msg.chunk
        data = bytes(buf)
        if len(data) != h.size or hashlib.sha256(data).hexdigest() != h.checksum_sha256:
            context.abort(grpc.StatusCode.INVALID_ARGUMENT, "checksum mismatch")
        self.blocks[h.block_id] = data
        replicas = [self.id]
        if h.pipeline:
            nxt, rest = h.pipeline[0], list(h.pipeline[1:])
            with grpc.insecure_channel(f"{nxt.host}:{nxt.port}") as ch:
                resp = pb_grpc.DataNodeStub(ch).WriteBlock(
                    _requests(h.block_id, data, h.checksum_sha256, rest))
            replicas += list(resp.replica_ids)
        return pb.WriteBlockResponse(block_id=h.block_id, checksum_sha256=h.checksum_sha256,
                                     bytes_written=len(data), replica_ids=replicas)

    def ReadBlock(self, request, context):
        if self.fail_reads:
            context.abort(grpc.StatusCode.UNAVAILABLE, "simulated outage")
        data = self.blocks.get(request.block_id)
        if data is None:
            context.abort(grpc.StatusCode.NOT_FOUND, "no such block")
        checksum = "f" * 64 if self.lie_checksum else hashlib.sha256(data).hexdigest()
        yield pb.ReadBlockResponse(header=pb.ReadBlockHeader(
            block_id=request.block_id, size=len(data), checksum_sha256=checksum))
        end = len(data) if request.length == 0 else min(len(data), request.offset + request.length)
        for i in range(request.offset, end, 64 * 1024):
            yield pb.ReadBlockResponse(chunk=data[i:min(i + 64 * 1024, end)])

    def DeleteBlock(self, request, context):
        return pb.DeleteBlockResponse(deleted=self.blocks.pop(request.block_id, None) is not None)


def _requests(block_id, data, checksum, pipeline):
    yield pb.WriteBlockRequest(header=pb.WriteBlockHeader(
        block_id=block_id, size=len(data), checksum_sha256=checksum, pipeline=pipeline))
    for i in range(0, len(data), 64 * 1024):
        yield pb.WriteBlockRequest(chunk=data[i:i + 64 * 1024])


# ---------------------------------------------------------------- NameNode

class FakeNameNode:
    """Metadatos mínimos: directorios, archivos -> bloques -> réplicas."""

    def __init__(self, datanodes: list[FakeDataNode], replication: int = 3,
                 token: str = "tok", standby: bool = False):
        self.datanodes = datanodes
        self.replication = replication
        self.token = token
        self.standby = standby
        # Nº de allocate en los que el NameNode aún no "se ha enterado" de que un
        # DataNode murió (simula el retraso del heartbeat). Permite probar el
        # reintento del cliente con un pipeline nuevo.
        self.stale_allocs = 0
        self.dirs: set[str] = {"/"}
        self.files: dict[str, dict] = {}  # path -> {"complete": bool, "blocks": [..]}
        self._rr = 0
        self._lock = threading.Lock()
        self._server = None
        self.port = 0
        self.calls: list[tuple[str, str]] = []

    def start(self) -> "FakeNameNode":
        nn = self

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *a):  # silencio
                pass

            def _send(self, status, payload=None):
                body = json.dumps(payload).encode() if payload is not None else b""
                self.send_response(status)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)

            def _body(self):
                n = int(self.headers.get("Content-Length") or 0)
                return json.loads(self.rfile.read(n) or b"{}")

            def _dispatch(self):
                url = urlparse(self.path)
                qs = {k: v[0] for k, v in parse_qs(url.query).items()}
                nn.calls.append((self.command, url.path))
                if nn.standby:
                    return self._send(503, {"error": "standby", "code": "STANDBY"})
                if url.path != "/auth/login":
                    if self.headers.get("Authorization") != f"Bearer {nn.token}":
                        return self._send(401, {"error": "unauthorized"})
                try:
                    status, payload = nn.handle(self.command, url.path, qs, self._body)
                except KeyError as e:
                    status, payload = 404, {"error": f"not found: {e}"}
                self._send(status, payload)

            do_GET = do_POST = do_DELETE = _dispatch

        self._server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.port = self._server.server_address[1]
        threading.Thread(target=self._server.serve_forever, daemon=True).start()
        return self

    def stop(self):
        if self._server:
            self._server.shutdown()

    @property
    def url(self) -> str:
        return f"http://127.0.0.1:{self.port}"

    # -- lógica
    def handle(self, method, path, qs, body):
        with self._lock:
            if path == "/auth/login":
                b = body()
                return 200, {"token": self.token, "home": f"/user/{b['username']}"}
            if path == "/fs/ls":
                p = qs["path"]
                if p in self.files:
                    return 200, {"type": "file", "name": p.rsplit("/", 1)[-1]}
                if p not in self.dirs:
                    return 404, {"error": "no existe"}
                entries = []
                for d in self.dirs:
                    if d != p and _parent(d) == p:
                        entries.append({"name": d.rsplit("/", 1)[-1], "type": "dir"})
                for f, meta in self.files.items():
                    if _parent(f) == p:
                        entries.append({"name": f.rsplit("/", 1)[-1], "type": "file",
                                        "size": sum(b["size"] for b in meta["blocks"])})
                return 200, {"type": "dir", "entries": entries}
            if path == "/fs/mkdir":
                p = body()["path"]
                if p in self.dirs:
                    return 409, {"error": "ya existe"}
                self.dirs.add(p)
                return 201, {}
            if path == "/fs/rmdir":
                self.dirs.discard(qs["path"])
                return 204, None
            if path == "/fs/rm":
                if self.files.pop(qs["path"], None) is None:
                    return 404, {"error": "no existe"}
                return 204, None
            if path == "/files/allocate":
                b = body()
                p = b["path"]
                meta = self.files.setdefault(p, {"complete": False, "blocks": []})
                if meta["complete"]:
                    return 409, {"error": "ya existe"}
                if self.stale_allocs > 0:
                    self.stale_allocs -= 1
                    alive = list(self.datanodes)
                else:
                    alive = [d for d in self.datanodes if d.alive]
                if len(alive) < self.replication:
                    return 503, {"error": "no hay suficientes DataNodes"}
                pipeline = [alive[(self._rr + i) % len(alive)].endpoint for i in range(self.replication)]
                self._rr += 1
                block_id = f"blk_{p.strip('/').replace('/', '_')}_{b['blockIndex']}_{self._rr}"
                return 200, {"blockId": block_id, "offset": b["offset"], "size": b["size"],
                             "pipeline": pipeline}
            if path == "/files/commit":
                b = body()
                replicas = b.get("replicaIds", [])
                if len(replicas) < self.replication:
                    return 409, {"error": "réplicas insuficientes"}
                meta = self.files[b["path"]]
                meta["blocks"] = [x for x in meta["blocks"] if x["blockId"] != b["blockId"]]
                meta["blocks"].append({"blockId": b["blockId"], "size": b["size"],
                                       "checksumSha256": b["checksumSha256"],
                                       "replicaIds": replicas})
                return 200, {}
            if path == "/files/finalize":
                self.files[body()["path"]]["complete"] = True
                return 200, {}
            if path == "/files/locations":
                meta = self.files[qs["path"]]
                if not meta["complete"]:
                    return 409, {"error": "en construcción"}
                by_id = {d.id: d for d in self.datanodes}
                blocks, offset = [], 0
                for i, blk in enumerate(meta["blocks"]):
                    locs = [by_id[r].endpoint for r in blk["replicaIds"] if r in by_id]
                    blocks.append({"index": i, "blockId": blk["blockId"], "size": blk["size"],
                                   "offset": offset, "checksumSha256": blk["checksumSha256"],
                                   "locations": locs})
                    offset += blk["size"]
                return 200, {"blocks": blocks}
            return 404, {"error": "ruta desconocida"}


def _parent(p: str) -> str:
    return p.rsplit("/", 1)[0] or "/"
