"""Local web control plane: a loopback-only HTTP server over a running Node.

Mirrors internal/web/server.go: same endpoints, same JSON shapes, same
loopback-only binding rule. HTTP peers on the LAN never connect here; they talk
to the swarm node directly. Served with the stdlib threaded HTTP server.
"""
from __future__ import annotations

import json
import os
import re
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from ipaddress import ip_address
from pathlib import Path
from typing import Optional

from . import dialog
from .node import Node

try:  # 3.9+
    import importlib.resources as resources
except ImportError:  # pragma: no cover
    import importlib_resources as resources  # type: ignore

_INDEX_CACHE: Optional[bytes] = None


def _index_bytes() -> bytes:
    global _INDEX_CACHE
    if _INDEX_CACHE is None:
        try:
            _INDEX_CACHE = resources.files("chuanshu").joinpath("index.html").read_bytes()
        except (FileNotFoundError, TypeError):  # dev tree fallback
            _INDEX_CACHE = (Path(__file__).with_name("index.html")).read_bytes()
    return _INDEX_CACHE


class _Handler(BaseHTTPRequestHandler):
    server_version = "chuanshu"
    protocol_version = "HTTP/1.1"
    node: Node  # set on the server class

    # ---- plumbing ----

    def log_message(self, *args) -> None:  # silence request logging
        pass

    def _json(self, code: int, obj) -> None:
        body = json.dumps(obj).encode("utf-8")
        self.send_response(code)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def _err(self, code: int, msg: str) -> None:
        self._json(code, {"error": msg})

    def _method_guard(self, want: str) -> bool:
        if self.command != want:
            self._err(405, f"{want} only")
            return False
        return True

    def _read_body(self) -> dict:
        length = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(length) if length else b"{}"
        try:
            data = json.loads(raw.decode("utf-8") or "{}")
            return data if isinstance(data, dict) else {}
        except (ValueError, UnicodeDecodeError):
            return {}

    # ---- routes ----

    def do_GET(self) -> None:  # noqa: N802
        path = self.path.split("?", 1)[0]
        try:
            if path == "/":
                body = _index_bytes()
                self.send_response(200)
                self.send_header("Content-Type", "text/html; charset=utf-8")
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)
            elif path == "/api/status":
                if self._method_guard("GET"):
                    files = [s.to_dict() for s in self.node.all_status()]
                    self._json(200, {"files": files})
            elif path == "/api/discover":
                if self._method_guard("GET"):
                    self._json(200, self._discover())
            else:
                self._err(404, "not found")
        except (BrokenPipeError, ConnectionResetError):
            pass

    def do_POST(self) -> None:  # noqa: N802
        path = self.path.split("?", 1)[0]
        try:
            if path == "/api/share" and self._method_guard("POST"):
                self._share()
            elif path == "/api/get" and self._method_guard("POST"):
                self._get()
            elif path == "/api/pick/file" and self._method_guard("POST"):
                self._pick(dialog.pick_file)
            elif path == "/api/pick/folder" and self._method_guard("POST"):
                self._pick(dialog.pick_folder)
            else:
                self._err(404, "not found")
        except (BrokenPipeError, ConnectionResetError):
            pass

    # ---- handlers ----

    def _discover(self) -> dict:
        entries = []
        for h in self.node.discovered_hashes():
            entry = {"hash": h, "name": "(未知)", "size": 0,
                     "pieces": 0, "seeders": self.node.seeder_count(h)}
            addr = self.node.find_peer(h)
            if addr:
                try:
                    meta = self.node.get_meta(addr, bytes.fromhex(h))
                    entry.update(name=meta.name, size=meta.length,
                                 pieces=meta.num_pieces())
                except Exception:
                    pass
            entries.append(entry)
        return {"files": entries}

    def _share(self) -> None:
        req = self._read_body()
        raw = (req.get("path") or "").strip()
        if not raw:
            self._err(400, "empty path")
            return
        path = os.path.expanduser(raw)
        if not os.path.isfile(path):
            self._err(400, f"file not accessible: {path}")
            return
        piece_kib = int(req.get("pieceKiB") or 256)
        try:
            meta = self.node.add_seed(path, piece_kib * 1024)
        except Exception as e:
            self._err(500, str(e))
            return
        self._json(200, {
            "infoHash": meta.info_hash_hex(),
            "name": meta.name,
            "length": meta.length,
            "pieces": meta.num_pieces(),
            "pieceLen": meta.piece_length,
        })

    def _get(self) -> None:
        req = self._read_body()
        raw_hash = (req.get("hash") or "").strip()
        if not re.fullmatch(r"[0-9a-fA-F]{64}", raw_hash):
            self._err(400, "hash must be 64 hex characters")
            return
        out = (req.get("out") or ".").strip() or "."
        out = os.path.expanduser(out)
        info_hash = bytes.fromhex(raw_hash)
        addr = self.node.find_peer(raw_hash)
        if not addr:
            self._err(404, "no seeder advertising this hash on the LAN yet")
            return
        try:
            meta = self.node.get_meta(addr, info_hash)
            self.node.start_download(out, meta)
        except Exception as e:
            self._err(502 if "metadata" in str(e) else 500, str(e))
            return
        self._json(200, {"infoHash": raw_hash.lower(),
                         "name": meta.name, "length": meta.length})

    def _pick(self, fn) -> None:
        try:
            path = fn()
        except dialog.CancelledError:
            self._json(200, {"path": ""})
        except dialog.NoPickerError:
            self._err(501, "no native picker on this platform")
        except Exception as e:
            self._err(500, str(e))
        else:
            self._json(200, {"path": path})


class Server:
    """Loopback-only HTTP control plane over a Node."""

    def __init__(self, node: Node, addr: str = "127.0.0.1:0") -> None:
        host, _, port = addr.partition(":")
        try:
            ip = ip_address(host)
        except ValueError:
            raise ValueError(f'web UI must bind a loopback address, got "{addr}"')
        if not ip.is_loopback:
            raise ValueError(f'web UI must bind a loopback address, got "{addr}"')
        self._node = node
        handler = type("Bound", (_Handler,), {"node": node})
        self._httpd = ThreadingHTTPServer((host, int(port or 0)), handler)
        self._httpd.daemon_threads = True
        self._thread = threading.Thread(target=self._httpd.serve_forever,
                                        daemon=True)

    def start(self) -> None:
        self._thread.start()

    def addr(self) -> str:
        host, port = self._httpd.server_address[:2]
        return f"{host}:{port}"

    def close(self) -> None:
        self._httpd.shutdown()
        self._httpd.server_close()
