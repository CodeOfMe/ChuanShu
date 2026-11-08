"""Integration tests for Node: end-to-end transfer between two live Nodes.

Each test picks a unique discovery port to avoid interfering with anything else
running on the well-known port.
"""
import os
import time

import pytest

from chuanshu import discovery
from chuanshu.node import Node


@pytest.fixture
def unique_port():
    return 51600 + (os.getpid() % 200)


def _make_src(tmp_path, size, name="data.bin"):
    src = tmp_path / name
    src.write_bytes(os.urandom(size))
    return src


def test_node_share_and_download(tmp_path, unique_port):
    src = _make_src(tmp_path, 5000)
    seeder = Node(discovery_port=unique_port)
    seeder.start()
    try:
        meta = seeder.add_seed(str(src), 1024)
        time.sleep(0.5)
        downloader_dir = tmp_path / "dl"
        downloader = Node(discovery_port=unique_port)
        downloader.start()
        try:
            # Wait briefly until the downloader sees the seeder in discovery.
            ihex = meta.info_hash_hex()
            deadline = time.time() + 10
            while time.time() < deadline and not downloader.find_peer(ihex):
                time.sleep(0.1)
            peer = downloader.find_peer(ihex)
            assert peer, "downloader did not discover seeder"
            dl_meta = downloader.get_meta(peer, bytes.fromhex(ihex))
            assert dl_meta.name == src.name
            assert dl_meta.info_hash_hex() == ihex
            downloader.start_download(str(downloader_dir), dl_meta)
            # Poll to completion.
            deadline = time.time() + 15
            while time.time() < deadline:
                st = downloader.status_for(ihex)
                if st and st.complete:
                    break
                time.sleep(0.1)
            st = downloader.status_for(ihex)
            assert st and st.complete
            out = downloader_dir / src.name
            assert out.exists() and out.read_bytes() == src.read_bytes()
        finally:
            downloader.stop()
    finally:
        seeder.stop()


def test_node_reports_progress_status(tmp_path, unique_port):
    src = _make_src(tmp_path, 5000)
    seeder = Node(discovery_port=unique_port)
    seeder.start()
    try:
        meta = seeder.add_seed(str(src), 1024)
        time.sleep(0.5)
        st = seeder.status_for(meta.info_hash_hex())
        assert st is not None and st.complete and st.pieces >= 1
    finally:
        seeder.stop()
