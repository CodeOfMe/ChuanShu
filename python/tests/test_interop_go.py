"""Cross-language interop tests: Python seeder <-> Go seeder.

These spawn the compiled Go binary as a subprocess and verify the two
implementations interoperate over live multicast discovery + TCP transfer. The
suite auto-skips when the ``chuanshu`` Go binary is not available (set CHUANSHU_GO
to point at it, or it is looked up next to this package / on PATH).
"""
import os
import shutil
import signal
import subprocess
import time
from pathlib import Path

import pytest

from chuanshu import store
from chuanshu.node import Node

GO_BIN = os.environ.get("CHUANSHU_GO")


def _find_go_bin():
    if GO_BIN and Path(GO_BIN).is_file():
        return GO_BIN
    found = shutil.which("chuanshu-go") or shutil.which("chuanshu")
    return found


go_binary = _find_go_bin()
requires_go = pytest.mark.skipif(not go_binary,
                                 reason="Go chuanshu binary not available "
                                        "(set CHUANSHU_GO)")

# The Go binary uses a fixed discovery port; the Python side has to meet it
# there, so these tests share port 51218 with the running peer.
UNIQUE_PORT = 51218


def _wait_file(path: Path, expected: bytes, timeout: float = 30.0) -> bool:
    deadline = time.time() + timeout
    while time.time() < deadline:
        if path.exists():
            try:
                if path.stat().st_size == len(expected):
                    return path.read_bytes() == expected
            except OSError:
                pass
        time.sleep(0.2)
    return False


def _terminate(proc):
    if proc.poll() is None:
        proc.send_signal(signal.SIGINT)
        try:
            proc.wait(timeout=5)
        except subprocess.TimeoutExpired:
            proc.kill()


@requires_go
def test_go_seeds_python_downloads(tmp_path):
    src = tmp_path / "goseed.bin"
    src.write_bytes(os.urandom(20000))

    go_env = dict(os.environ)
    seeder = subprocess.Popen([go_binary, "share", str(src), "--piece", "1"],
                              stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                              env=go_env)
    downloader = Node(discovery_port=UNIQUE_PORT)
    downloader.start()
    try:
        time.sleep(1.5)
        ihex = store.build_metadata(str(src), 1024).info_hash_hex()
        # Cross-language identity must match: Python computes the same hash the
        # Go seeder advertises.
        deadline = time.time() + 12
        while time.time() < deadline and not downloader.find_peer(ihex):
            time.sleep(0.1)
        peer = downloader.find_peer(ihex)
        assert peer, "Python did not discover Go seeder"

        meta = downloader.get_meta(peer, bytes.fromhex(ihex))
        assert meta.info_hash_hex() == ihex

        out = tmp_path / "out"
        downloader.start_download(str(out), meta)
        assert _wait_file(out / src.name, src.read_bytes(), timeout=30)
    finally:
        downloader.stop()
        _terminate(seeder)


@requires_go
def test_python_seeds_go_downloads(tmp_path):
    src = tmp_path / "pyseed.bin"
    src.write_bytes(os.urandom(20000))
    ihex = store.build_metadata(str(src), 1024).info_hash_hex()

    seeder = Node(discovery_port=UNIQUE_PORT)
    seeder.start()
    seeder.add_seed(str(src), 1024)
    out = tmp_path / "out"
    out.mkdir()
    go_env = dict(os.environ)
    go_proc = subprocess.Popen(
        [go_binary, "get", ihex, "--out", str(out)],
        stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, env=go_env)
    try:
        assert _wait_file(out / src.name, src.read_bytes(), timeout=30)
    finally:
        _terminate(go_proc)
        seeder.stop()


@requires_go
def test_hash_identical_across_languages(tmp_path):
    src = tmp_path / "中文名.bin"  # non-ASCII name stress
    src.write_bytes(os.urandom(12345))
    py_hash = store.build_metadata(str(src), 1024).info_hash_hex()
    out = subprocess.run([go_binary, "hash", str(src), "--piece", "1"],
                         capture_output=True, text=True, timeout=20)
    go_hash = out.stdout.strip()
    assert go_hash == py_hash
