import hashlib

from client.core.chunker import count_blocks, iter_chunks

BS = 1024


def _write(tmp_path, size):
    p = tmp_path / "f.bin"
    p.write_bytes(bytes(i % 251 for i in range(size)))
    return p


def test_empty_file(tmp_path):
    p = _write(tmp_path, 0)
    assert list(iter_chunks(p, BS)) == []
    assert count_blocks(p, BS) == 0


def test_smaller_than_block(tmp_path):
    p = _write(tmp_path, 100)
    chunks = list(iter_chunks(p, BS))
    assert len(chunks) == 1 and count_blocks(p, BS) == 1
    assert chunks[0].index == 0 and chunks[0].offset == 0 and len(chunks[0].data) == 100
    assert chunks[0].sha256 == hashlib.sha256(p.read_bytes()).hexdigest()


def test_exactly_two_blocks(tmp_path):
    p = _write(tmp_path, 2 * BS)
    chunks = list(iter_chunks(p, BS))
    assert [c.index for c in chunks] == [0, 1]
    assert [c.offset for c in chunks] == [0, BS]
    assert all(len(c.data) == BS for c in chunks)
    assert count_blocks(p, BS) == 2


def test_two_and_a_half_blocks(tmp_path):
    p = _write(tmp_path, 2 * BS + BS // 2)
    chunks = list(iter_chunks(p, BS))
    assert len(chunks) == 3 and count_blocks(p, BS) == 3
    assert len(chunks[-1].data) == BS // 2
    assert b"".join(c.data for c in chunks) == p.read_bytes()
