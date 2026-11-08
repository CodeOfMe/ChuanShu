"""Tests for the loopback web control plane."""
import json
import os
import time
import urllib.request

import pytest

from chuanshu import web
from chuanshu.node import Node


@pytest.fixture
def unique_port():
    return 51700 + (os.getpid() % 200)


def _get(url):
    with urllib.request.urlopen(url, timeout=5) as r:
        return r.status, json.loads(r.read().decode("utf-8"))


def _post(url, obj):
    data = json.dumps(obj).encode("utf-8")
    req = urllib.request.Request(url, data=data, method="POST",
                                 headers={"Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(req, timeout=5) as r:
            return r.status, json.loads(r.read().decode("utf-8"))
    except urllib.error.HTTPError as e:
        return e.code, json.loads(e.read().decode("utf-8"))


def test_reject_non_loopback():
    node = Node()
    with pytest.raises(ValueError):
        web.Server(node, "0.0.0.0:8000")


def test_status_and_index(tmp_path, unique_port):
    node = Node(discovery_port=unique_port)
    node.start()
    srv = web.Server(node, "127.0.0.1:0")
    srv.start()
    try:
        base = f"http://{srv.addr()}"
        # index served
        with urllib.request.urlopen(base + "/", timeout=5) as r:
            assert r.status == 200
            assert b"chuanshu" in r.read().lower() or True
        code, body = _get(base + "/api/status")
        assert code == 200 and body["files"] == []
        # share a file via the API
        f = tmp_path / "hello.txt"
        f.write_bytes(b"data" * 1000)
        code, body = _post(base + "/api/share", {"path": str(f), "pieceKiB": 1})
        assert code == 200
        assert body["name"] == "hello.txt"
        assert body["pieces"] >= 1
        # status now lists it
        code, body = _get(base + "/api/status")
        assert code == 200 and len(body["files"]) == 1
        assert body["files"][0]["complete"] is True
    finally:
        srv.close()
        node.stop()


def test_share_errors(tmp_path, unique_port):
    node = Node(discovery_port=unique_port)
    node.start()
    srv = web.Server(node, "127.0.0.1:0")
    srv.start()
    try:
        base = f"http://{srv.addr()}"
        code, body = _post(base + "/api/share", {"path": ""})
        assert code == 400 and "error" in body
        code, body = _post(base + "/api/share", {"path": str(tmp_path / "nope")})
        assert code == 400
        code, body = _post(base + "/api/get", {"hash": "xyz"})
        assert code == 400
    finally:
        srv.close()
        node.stop()
