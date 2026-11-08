"""Command line interface: share / get / ls / hash / serve / version.

Mirrors the Go cmd/chuanshu. Running with no arguments (e.g. launching the
tool) opens the web UI and prints its URL, so the common case is zero-config.
"""
from __future__ import annotations

import argparse
import sys
import time
from pathlib import Path

from . import __version__, protocol, store
from .node import Node


def _human_bytes(n: int) -> str:
    unit = 1024.0
    if n < unit:
        return f"{int(n)} B"
    value = float(n)
    for suffix in ("KiB", "MiB", "GiB", "TiB", "PiB", "EiB"):
        value /= unit
        if value < unit:
            return f"{value:.1f} {suffix}"
    return f"{value:.1f} EiB"


def _make_node() -> Node:
    node = Node()
    node.start()
    return node


def _share(path: str, piece_kib: int) -> None:
    node = _make_node()
    try:
        meta = node.add_seed(path, piece_kib * 1024)
        print(f"Sharing {Path(path).name} ({_human_bytes(meta.length)})")
        print(f"  info hash : {meta.info_hash_hex()}")
        print(f"  pieces    : {meta.num_pieces()} x {_human_bytes(meta.piece_length)}")
        print(f"  from      : {node.my_addr()}")
        print("\nOthers on this LAN can download it with:")
        print(f"    chuanshu get {meta.info_hash_hex()}")
        print("\nPress Ctrl-C to stop sharing.")
        _wait_forever()
    finally:
        node.stop()


def _get(info_hex: str, out_dir: str, wait: int) -> None:
    node = _make_node()
    try:
        print(f"Looking for {info_hex} on the local network...")
        info_hash = bytes.fromhex(info_hex)
        deadline = time.time() + wait
        meta = None
        while meta is None:
            addr = node.find_peer(info_hex)
            if addr:
                try:
                    meta = node.get_meta(addr, info_hash)
                except Exception:
                    meta = None
            if meta is None:
                if time.time() > deadline:
                    print(f"error: no seeder advertising {info_hex} found within {wait}s",
                          file=sys.stderr)
                    sys.exit(1)
                time.sleep(0.5)
        print(f"Found seeder: {meta.name} ({_human_bytes(meta.length)}, "
              f"{meta.num_pieces()} pieces)")
        node.start_download(out_dir, meta)
        target = None
        while True:
            st = node.status_for(info_hex)
            if st is None:
                break
            pct = st.progress * 100
            print(f"  {pct:6.2f}%  ({st.have_pieces}/{st.pieces} pieces, "
                  f"{st.peers} peers, down {_human_bytes(st.downloaded)})"
                  , end="\r", flush=True)
            if st.complete:
                target = str(Path(out_dir) / st.name)
                break
            time.sleep(0.4)
        print()
        if target:
            print(f"\nDone: {target}")
            print("Press Ctrl-C to stop seeding (or keep this running to help others).")
            _wait_forever()
    finally:
        node.stop()


def _ls(wait: int) -> None:
    node = _make_node()
    try:
        print(f"Listening on the local network for {wait}s...")
        time.sleep(wait)
        hashes = node.discovered_hashes()
        if not hashes:
            print("No shared files found on the LAN yet.")
            return
        print(f"{len(hashes)} file(s) shared on the LAN:\n")
        print(f"{'SEEDERS':<8}{'SIZE':<10}{'NAME':<24}INFO HASH")
        for h in sorted(hashes):
            seeds = node.seeder_count(h)
            name, size = "-", "-"
            addr = node.find_peer(h)
            if addr:
                try:
                    m = node.get_meta(addr, bytes.fromhex(h))
                    name = m.name
                    size = _human_bytes(m.length)
                except Exception:
                    pass
            print(f"{seeds:<8}{size:<10}{name:<24}{h}")
    finally:
        node.stop()


def _hash(path: str, piece_kib: int) -> None:
    try:
        meta = store.build_metadata(path, piece_kib * 1024)
    except (OSError, ValueError, protocol.ProtocolError) as e:
        print(f"error: {e}", file=sys.stderr)
        sys.exit(1)
    print(meta.info_hash_hex())


def _wait_forever() -> None:
    try:
        while True:
            time.sleep(3600)
    except KeyboardInterrupt:
        print("\nStopping.")


def _serve(addr: str, no_open: bool) -> None:
    from . import web
    node = _make_node()
    srv = None
    try:
        srv = web.Server(node, addr)
        srv.start()
        url = f"http://{srv.addr()}/"
        print(f"传书 UI 已启动: {url}")
        print(f"节点对外地址: {node.my_addr()}")
        if not no_open:
            _open_browser(url)
        print("保持此终端运行;按 Ctrl-C 退出。")
        _wait_forever()
    finally:
        if srv is not None:
            srv.close()
        node.stop()


def _open_browser(url: str) -> None:
    import subprocess
    import sys as _sys
    try:
        if _sys.platform == "darwin":
            subprocess.Popen(["open", url])
        elif _sys.platform.startswith("win"):
            import webbrowser
            webbrowser.open(url)
        else:
            subprocess.Popen(["xdg-open", url])
    except Exception:
        pass


_USAGE = """chuanshu 传书 - LAN P2P file transfer (Python)

Usage:
  chuanshu                              Open the web UI (default)
  chuanshu serve [--addr 127.0.0.1:0]   Start local web UI
  chuanshu share <file> [--piece KiB]   Share a file and seed it
  chuanshu get   <hash> [--out DIR]     Fetch a file by its info hash
  chuanshu ls    [--wait S]             List files shared on the LAN
  chuanshu hash  <file> [--piece KiB]   Print a file's info hash
  chuanshu version                      Show version
"""


def main(argv: list | None = None) -> None:
    args = list(sys.argv[1:] if argv is None else argv)
    if not args:
        _serve("127.0.0.1:0", no_open=False)
        return

    cmd = args[0]
    rest = args[1:]
    if cmd in ("version", "-v", "--version"):
        print(f"chuanshu {__version__}")
        return
    if cmd in ("help", "-h", "--help"):
        print(_USAGE)
        return

    if cmd == "serve":
        p = argparse.ArgumentParser(prog="chuanshu serve")
        p.add_argument("--addr", default="127.0.0.1:0")
        p.add_argument("--no-open", action="store_true")
        ns = p.parse_args(rest)
        try:
            _serve(ns.addr, ns.no_open)
        except ValueError as e:
            print(f"error: {e}", file=sys.stderr)
            sys.exit(1)
    elif cmd == "share":
        p = argparse.ArgumentParser(prog="chuanshu share")
        p.add_argument("file")
        p.add_argument("--piece", type=int, default=256)
        ns = p.parse_args(rest)
        _share(ns.file, ns.piece)
    elif cmd == "get":
        p = argparse.ArgumentParser(prog="chuanshu get")
        p.add_argument("hash")
        p.add_argument("--out", default=".")
        p.add_argument("--wait", type=int, default=120)
        ns = p.parse_args(rest)
        _get(ns.hash, ns.out, ns.wait)
    elif cmd == "ls":
        p = argparse.ArgumentParser(prog="chuanshu ls")
        p.add_argument("--wait", type=int, default=4)
        ns = p.parse_args(rest)
        _ls(ns.wait)
    elif cmd == "hash":
        p = argparse.ArgumentParser(prog="chuanshu hash")
        p.add_argument("file")
        p.add_argument("--piece", type=int, default=256)
        ns = p.parse_args(rest)
        _hash(ns.file, ns.piece)
    else:
        print(f"unknown command: {cmd}\n", file=sys.stderr)
        print(_USAGE, file=sys.stderr)
        sys.exit(2)


if __name__ == "__main__":
    main()
