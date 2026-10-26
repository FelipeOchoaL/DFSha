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
