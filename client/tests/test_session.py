from client.core.session import Session


def test_resolve_paths():
    s = Session(cwd="/user/ana")
    assert s.resolve("docs") == "/user/ana/docs"
    assert s.resolve("./docs/../a.txt") == "/user/ana/a.txt"
    assert s.resolve("/abs/x") == "/abs/x"
    assert s.resolve("..") == "/user"
    assert s.resolve("../../..") == "/"
    assert Session(cwd="/").resolve("a//b") == "/a/b"


def test_save_and_load_roundtrip(tmp_path):
    path = tmp_path / "session.json"
    s = Session(path=path, token="t", username="ana", cwd="/user/ana", active_namenode="http://nn")
    s.save()
    loaded = Session.load(path)
    assert (loaded.token, loaded.username, loaded.cwd, loaded.active_namenode) == \
        ("t", "ana", "/user/ana", "http://nn")


def test_load_missing_or_corrupt(tmp_path):
    assert Session.load(tmp_path / "nope.json").cwd == "/"
    bad = tmp_path / "bad.json"
    bad.write_text("{not json")
    assert Session.load(bad).token is None
