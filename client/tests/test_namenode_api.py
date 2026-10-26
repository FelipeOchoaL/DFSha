from unittest.mock import patch

import pytest
import requests

from client.core.errors import NameNodeError, NameNodeUnavailableError
from client.core.namenode_api import NameNodeAPI, pick


class _Resp:
    def __init__(self, status, payload=None, content=b"{}"):
        self.status_code = status
        self._payload = payload
        self.content = content if payload is None else b"x"
        self.reason = "reason"

    def json(self):
        if self._payload is None:
            raise ValueError
        return self._payload


def test_pick_camel_and_snake():
    assert pick({"blockId": 1}, "blockId", "block_id") == 1
    assert pick({"block_id": 2}, "blockId", "block_id") == 2
    assert pick({}, "x", default="d") == "d"


def test_sends_bearer_and_parses_json():
    api = NameNodeAPI(["http://a"], token="tok")
    with patch.object(requests.Session, "request", return_value=_Resp(200, {"ok": True})) as m:
        assert api.ls("/") == {"ok": True}
    kwargs = m.call_args.kwargs
    assert kwargs["headers"]["Authorization"] == "Bearer tok"
    assert m.call_args.args == ("GET", "http://a/fs/ls")
    assert kwargs["params"] == {"path": "/"}


def test_error_payload_becomes_exception():
    api = NameNodeAPI(["http://a"])
    with patch.object(requests.Session, "request",
                      return_value=_Resp(409, {"error": "ya existe", "code": "EXISTS"})):
        with pytest.raises(NameNodeError) as ei:
            api.mkdir("/x")
    assert ei.value.status == 409 and ei.value.code == "EXISTS" and "ya existe" in str(ei.value)


def test_failover_on_connection_error_and_503():
    api = NameNodeAPI(["http://a", "http://b", "http://c"])
    responses = {
        "http://a/fs/ls": requests.ConnectionError(),
        "http://b/fs/ls": _Resp(503, {"error": "standby"}),
        "http://c/fs/ls": _Resp(200, {"entries": []}),
    }

    def fake(method, url, **kw):
        r = responses[url]
        if isinstance(r, Exception):
            raise r
        return r

    with patch.object(requests.Session, "request", side_effect=fake):
        assert api.ls("/") == {"entries": []}
    assert api.active_url == "http://c"


def test_all_down_raises_unavailable():
    api = NameNodeAPI(["http://a", "http://b"])
    with patch.object(requests.Session, "request", side_effect=requests.Timeout()):
        with pytest.raises(NameNodeUnavailableError):
            api.ls("/")


def test_preferred_url_goes_first():
    api = NameNodeAPI(["http://a", "http://b"], preferred="http://b")
    assert api.active_url == "http://b"


def test_allocate_opens_then_continues_the_same_upload():
    api = NameNodeAPI(["http://a"])
    with patch.object(requests.Session, "request", return_value=_Resp(200, {"uploadId": "up-1"})) as request:
        assert api.allocate("/file", 0, 0, 10)["uploadId"] == "up-1"
        assert "uploadId" not in request.call_args.kwargs["json"]
        api.allocate("/file", 1, 10, 5, upload_id="up-1")
        assert request.call_args.kwargs["json"] == {
            "path": "/file", "blockIndex": 1, "offset": 10, "size": 5, "uploadId": "up-1"}


@pytest.mark.parametrize("operation", ["commit", "finalize"])
def test_write_operations_send_the_upload_id(operation):
    api = NameNodeAPI(["http://a"])
    with patch.object(requests.Session, "request", return_value=_Resp(200, {"ok": True})) as request:
        if operation == "commit":
            api.commit("/file", "block-1", 10, "a" * 64,
                       replica_ids=("dn1", "dn2", "dn3"), upload_id="up-1")
        else:
            api.finalize("/file", upload_id="up-1")
    assert request.call_args.args == ("POST", f"http://a/files/{operation}")
    assert request.call_args.kwargs["json"]["uploadId"] == "up-1"


def test_503_without_standby_code_fails_over():
    api = NameNodeAPI(["http://a", "http://b"])
    responses = {
        "http://a/fs/ls": _Resp(503, {"error": "standby", "code": "NAMENODE_STANDBY"}),
        "http://b/fs/ls": _Resp(200, {"entries": []}),
    }
    with patch.object(requests.Session, "request", side_effect=lambda m, u, **kw: responses[u]):
        assert api.ls("/") == {"entries": []}
    assert api.active_url == "http://b"


def test_insufficient_datanodes_does_not_fail_over():
    api = NameNodeAPI(["http://a", "http://b"])
    seen = []

    def fake(method, url, **kw):
        seen.append(url)
        return _Resp(503, {"error": "faltan DataNodes", "code": "INSUFFICIENT_DATANODES"})

    with patch.object(requests.Session, "request", side_effect=fake):
        with pytest.raises(NameNodeError) as ei:
            api.ls("/")
    assert ei.value.status == 503 and ei.value.code == "INSUFFICIENT_DATANODES"
    assert seen == ["http://a/fs/ls"]


def test_recovering_retries_the_same_url_then_succeeds(monkeypatch):
    monkeypatch.setattr("client.core.namenode_api._sleep", lambda s: None)
    api = NameNodeAPI(["http://a", "http://b"])
    calls = []

    def fake(method, url, **kw):
        calls.append(url)
        if len(calls) < 3:
            return _Resp(503, {"error": "recuperando", "code": "NAMENODE_RECOVERING"})
        return _Resp(200, {"entries": []})

    with patch.object(requests.Session, "request", side_effect=fake):
        assert api.ls("/") == {"entries": []}
    assert calls == ["http://a/fs/ls"] * 3


def test_recovering_gives_up_after_the_retry_limit(monkeypatch):
    from client.core import namenode_api

    monkeypatch.setattr(namenode_api, "_sleep", lambda s: None)
    api = NameNodeAPI(["http://a"])
    with patch.object(requests.Session, "request",
                      return_value=_Resp(503, {"error": "recuperando", "code": "NAMENODE_RECOVERING"})) as m:
        with pytest.raises(Exception):
            api.ls("/")
    assert m.call_count <= namenode_api.RECOVERY_RETRIES + 2