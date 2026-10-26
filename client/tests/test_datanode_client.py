import hashlib

import pytest

from client.core.datanode_client import DataNodeClient
from client.core.errors import BlockCorruptedError, BlockNotFoundError, DataNodeUnavailableError
from client.tests.fakes import FakeDataNode


@pytest.fixture
def cluster():
    reg = {}
    nodes = [FakeDataNode(f"dn{i}", rack="A" if i < 2 else "B", registry=reg).start() for i in range(3)]
    yield nodes
    for n in nodes:
        n.stop()


def test_write_with_pipeline_replicates_to_all(cluster):
    dn1, dn2, dn3 = cluster
    data = bytes(range(256)) * 1000  # 256 KB -> varios chunks de 64 KB
    sha = hashlib.sha256(data).hexdigest()
    client = DataNodeClient(timeout=10)

    res = client.write_block(dn1.endpoint, "blk_1", data, sha, pipeline=[dn2.endpoint, dn3.endpoint])

    assert res.replica_ids == ("dn0", "dn1", "dn2")
    assert res.bytes_written == len(data) and res.checksum_sha256 == sha
    assert dn1.blocks["blk_1"] == dn2.blocks["blk_1"] == dn3.blocks["blk_1"] == data


def test_read_roundtrip_and_partial(cluster):
    dn1 = cluster[0]
    data = b"hola mundo" * 10000
    sha = hashlib.sha256(data).hexdigest()
    client = DataNodeClient(timeout=10)
    client.write_block(dn1.endpoint, "blk_r", data, sha)

    assert client.read_block(dn1.endpoint, "blk_r") == data
    assert client.read_block(dn1.endpoint, "blk_r", offset=4, length=6) == data[4:10]


def test_wrong_checksum_is_rejected(cluster):
    dn1 = cluster[0]
    client = DataNodeClient(timeout=10)
    with pytest.raises(Exception):
        client.write_block(dn1.endpoint, "blk_bad", b"abc", "0" * 64)
    assert "blk_bad" not in dn1.blocks


def test_errors_are_translated(cluster):
    dn1 = cluster[0]
    client = DataNodeClient(timeout=3)
    with pytest.raises(BlockNotFoundError):
        client.read_block(dn1.endpoint, "nope")

    dead = {"id": "dnX", "host": "127.0.0.1", "port": 1, "rack": "A"}
    with pytest.raises(DataNodeUnavailableError):
        client.read_block(dead, "blk")


def test_corrupted_block_detected_on_read(cluster):
    dn1 = cluster[0]
    client = DataNodeClient(timeout=10)
    data = b"x" * 1000
    client.write_block(dn1.endpoint, "blk_c", data, hashlib.sha256(data).hexdigest())
    # Si el nodo anuncia un checksum que no corresponde a los bytes, el cliente lo detecta.
    dn1.lie_checksum = True
    with pytest.raises(BlockCorruptedError):
        client.read_block(dn1.endpoint, "blk_c")
