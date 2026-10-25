"""put/get/ls/mkdir/rm de punta a punta contra NameNode y DataNodes falsos.

No requiere Go ni Docker. Demuestra que el flujo del cliente (chunker ->
allocate -> WriteBlock pipeline -> commit -> finalize -> locations -> ReadBlock)
funciona contra los contratos acordados.
"""
import hashlib
import os

import pytest

from client import main as cli
from client.tests.fakes import FakeDataNode, FakeNameNode


@pytest.fixture
def env(tmp_path, monkeypatch):
    reg = {}
    dns = [FakeDataNode(f"dn{i}", rack="A" if i < 2 else "B", registry=reg).start() for i in range(4)]
    nn = FakeNameNode(dns, replication=3).start()
    standby = FakeNameNode(dns, standby=True).start()

    monkeypatch.setenv("DFSHA_NAMENODES", f"{standby.url},{nn.url}")  # standby primero: prueba failover
    monkeypatch.setenv("DFSHA_BLOCK_SIZE_MB", "0.25")  # 256 KB
    monkeypatch.setenv("DFSHA_SESSION_FILE", str(tmp_path / "session.json"))
    monkeypatch.setattr("getpass.getpass", lambda *_: "secret")

    yield {"nn": nn, "dns": dns, "tmp": tmp_path}
    nn.stop()
    standby.stop()
    for d in dns:
        d.stop()


def _run(*argv):
    return cli.main(list(argv))


def test_full_cycle(env, capsys):
    tmp, nn, dns = env["tmp"], env["nn"], env["dns"]

    assert _run("login", "ana") == 0
    assert _run("pwd") == 0
    assert capsys.readouterr().out.strip().endswith("/user/ana")

    assert _run("mkdir", "-p", "/user/ana") == 0
    assert _run("mkdir", "docs") == 0
    assert _run("cd", "docs") == 0

    # 2.5 bloques de 256 KB
    src = tmp / "src.bin"
    src.write_bytes(os.urandom(256 * 1024 * 2 + 100_000))
    assert _run("put", str(src), "a.bin") == 0

    meta = nn.files["/user/ana/docs/a.bin"]
    assert meta["complete"] and len(meta["blocks"]) == 3
    # Partición: cada bloque está en exactamente 3 réplicas, en nodos distintos.
    for blk in meta["blocks"]:
        assert len(set(blk["replicaIds"])) == 3
        holders = [d.id for d in dns if blk["blockId"] in d.blocks]
        assert sorted(holders) == sorted(blk["replicaIds"])
    # Partición: los bloques del archivo no empiezan todos en el mismo nodo
    # (la colocación real con racks es de David; aquí basta ver que se reparte).
    primaries = [b["replicaIds"][0] for b in meta["blocks"]]
    assert len(set(primaries)) > 1

    assert _run("ls") == 0
    assert "a.bin" in capsys.readouterr().out

    dst = tmp / "dst.bin"
    assert _run("get", "a.bin", str(dst)) == 0
    assert hashlib.sha256(dst.read_bytes()).digest() == hashlib.sha256(src.read_bytes()).digest()

    assert _run("rm", "a.bin") == 0
    assert "/user/ana/docs/a.bin" not in nn.files


def test_get_survives_datanode_failure(env):
    tmp, nn, dns = env["tmp"], env["nn"], env["dns"]
    _run("login", "ana")
    src = tmp / "s.bin"
    src.write_bytes(os.urandom(600 * 1024))
    assert _run("put", str(src), "/user/ana/s.bin") == 0

    # Apaga el primer nodo que tenga el primer bloque.
    first = nn.files["/user/ana/s.bin"]["blocks"][0]["replicaIds"][0]
    next(d for d in dns if d.id == first).fail_reads = True

    dst = tmp / "d.bin"
    assert _run("get", "/user/ana/s.bin", str(dst)) == 0
    assert dst.read_bytes() == src.read_bytes()


def test_put_retries_when_pipeline_node_is_down(env):
    tmp, nn, dns = env["tmp"], env["nn"], env["dns"]
    _run("login", "ana")
    dns[1].stop()          # quedan 3 vivos: justo el factor de réplica
    nn.stale_allocs = 1    # el primer allocate aún incluye al nodo muerto en el pipeline
    src = tmp / "r.bin"
    src.write_bytes(os.urandom(300 * 1024))
    assert _run("put", str(src), "/user/ana/r.bin") == 0
    meta = nn.files["/user/ana/r.bin"]
    assert meta["complete"]
    assert all("dn1" not in b["replicaIds"] for b in meta["blocks"])
    # Hubo más allocate que bloques: el cliente reintentó con un pipeline nuevo.
    allocs = sum(1 for m, p in nn.calls if p == "/files/allocate")
    assert allocs > len(meta["blocks"])


def test_unauthenticated_gets_clear_error(env, capsys):
    assert _run("ls", "/") == 1
    assert "login" in capsys.readouterr().err
