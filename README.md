# 传书 · chuanshu

**Zero-config LAN peer-to-peer file transfer.** Start it on the machine that has
a (large) file, and everyone else on the same local network downloads it
*from each other* — not from you. Every downloader automatically becomes a
seeder, so the more people pull a file, the faster it spreads.

`chuanshu` (传书, "to send a letter") shares files over a local network using a
BitTorrent-style swarm: a file is split into fixed-size pieces, each piece is
SHA-256 verified, and peers exchange pieces directly. There is no central
server, no accounts, and nothing to install on the other machines beyond this
one binary.

```
     ┌──────────┐   share big.iso    ┌──────────┐
     │  host A  │◀──────────────────▶│ peer B   │  downloads pieces from A
     └────┬─────┘   p2p TCP pieces    └────┬─────┘
          │  ▲                             │  ▼  becomes a seeder too
          │  │ discovery (UDP multicast)   │
     ┌────┴──┴──────┐               ┌──────┴─────┐
     │    peer C    │◀─────────────▶│   peer D   │  pulls from A, B, C ...
     └──────────────┘   p2p TCP      └────────────┘
```

## Features

- **No server, no config.** Peers find each other automatically via UDP
  multicast on the LAN. No tracker, no ports to forward, no IPs to type.
- **Genuinely P2P.** Pieces come from every peer that already has them, so
  download bandwidth scales with the number of participants instead of
  bottlenecking on the original host.
- **Integrity guaranteed.** Every piece is verified against its SHA-256 hash
  before it is accepted, and the whole file is identified by a single
  content-addressed *info hash*. A bad peer is dropped immediately.
- **Resumable.** An interrupted download keeps its partial `.part` file; restart
  the command and it picks up only the missing pieces.
- **Tiny.** A single static binary with one small dependency. No daemon.

## Install

`chuanshu` is a single Go binary.

```sh
# From source (Go 1.26+)
go install github.com/CodeOfMe/ChuanShu/cmd/chuanshu@latest

# Or build in the repo
cd chuanshu
go build -o chuanshu ./cmd/chuanshu
```

A pure-Python implementation lives in [`python/`](python/) and is
**wire-compatible** with the Go version: a Python peer and a Go peer share the
same info hash for a file and can seed each other directly. It has zero runtime
dependencies (standard library only) and is published to PyPI.

```sh
pipx install chuanshu     # or: pip install chuanshu
```

See [`python/README.md`](python/README.md) for details.

## Usage

A file is identified by its **info hash** (a short hex digest of its content).
Get it on the host, then hand that hash to anyone on the LAN — or let them
discover it themselves with `ls`.

### 1. Share a file (seed it)

```sh
chuanshu share bigfile.iso
```

```
Sharing bigfile.iso (4.2 GiB)
  info hash : 3f2a...9c1d
  pieces    : 17203 x 256.0 KiB
  from      : 192.168.1.20:51812

Others on this LAN can download it with:
    chuanshu get 3f2a...9c1d
```

Keep this running; it seeds pieces to every peer that asks. Press `Ctrl-C` to
stop sharing.

### 2. Download a file by its info hash

```sh
chuanshu get 3f2a...9c1d --out ./incoming
```

```
Found seeder 192.168.1.20:51812: bigfile.iso (4.2 GiB, 17203 pieces)
Downloading into ./incoming
  42.13%  (7248/17203 pieces, 4 peers, down 1.8 GiB)
  100.00%  (17203/17203 pieces, 6 peers, down 4.2 GiB)

Done: ./incoming/bigfile.iso
You are now seeding it to other peers on this LAN.
```

As soon as a peer finishes, it advertises itself as a seeder, so the next
downloaders pull from an ever-growing set of sources.

### 3. Browse what is shared on the LAN

```sh
chuanshu ls
```

```
SEEDERS  SIZE      NAME          INFO HASH
2        4.2 GiB   bigfile.iso   3f2a...9c1d
1        180 MiB   demo.mp4      8b0c...77e2
```

### 4. Just compute a file's info hash

```sh
chuanshu hash bigfile.iso
```

### 5. Optional: local web UI

If a terminal is not your thing, run the same node as a background service and
open a browser tab to drive it.

```sh
chuanshu                    # no args: starts the node + UI and opens your browser
chuanshu serve              # equivalent; prints a URL like http://127.0.0.1:52345/
```

Double-clicking the binary does the same thing as running it with no arguments:
it starts a node, binds the loopback UI, and immediately opens your default
browser to it so you can start sharing right away.

The UI lets you pick a file to share (via the native OS file dialog, or type
the path directly), paste a hash to download, choose a destination folder, and
watch progress bars as pieces flow. The interface is available in **four
languages** (中文 / English / 日本語 / Русский), auto-detected from your browser
and switchable from the header. The HTTP server binds **loopback only** — peers
on the LAN still reach the node directly over the same UDP-multicast + TCP swarm
transport the CLI uses, and nothing is exposed to the network.

### Options

| Command | Flag | Default | Meaning |
| ------- | ---- | ------- | ------- |
| `share` | `--piece <KiB>` | `256` | piece size in KiB |
| `get`   | `--out <dir>`   | `.`   | output directory |
| `ls`    | `--wait <s>`    | `4`   | seconds to listen for peers |
| `serve` | `--addr <h:p>`  | `127.0.0.1:0` | loopback address to bind the UI (port 0 = auto) |
| `serve` | `--no-open`     | off   | do not auto-open a browser window |

Flags work in any position *after* the subcommand, e.g. `chuanshu get <hash> --out out` or
`chuanshu get --out out <hash>`.

## How it works

- **Chunking + content addressing.** `share` splits the file into fixed-size
  pieces and hashes each one with SHA-256. The info hash is the hash of that
  piece-hash list (plus name/size/piece size), so two peers agree on "the same
  file" from the hash alone.
- **Discovery.** Every peer joins an IPv4 multicast group and periodically
  announces "I have (a part of) hash X on TCP port P". A newcomer learns the
  whole set of interested peers within a few seconds, with zero configuration.
  Peer TCP ports are ephemeral, so many instances run fine on one machine.
- **Transfer.** Peers connect over TCP and request pieces by index. A peer only
  serves pieces it has already verified (tracked in its bitfield), and a peer
  receiving a piece verifies it against its expected hash before writing it to
  disk — a mismatch drops that peer.
- **Seeding.** There is no distinction between "the uploader" and "a
  downloader": anyone holding valid pieces serves them, which is what makes the
  swarm speed up as it grows.

A partial download lives at `<name>.part` at full length and pieces are written
at their offsets, so arrival order does not matter and resume re-verifies what
is already on disk.

## Requirements

- Go 1.26+ to build (only dependency: `golang.org/x/sys`).
- The tool uses IPv4 multicast for discovery and ephemeral TCP ports for data.
  On some systems the OS firewall may prompt for network access the first time.
- Peer discovery relies on multicast being allowed on the local network. This is
  standard on home/office LANs but is often blocked on "guest"/client-isolated
  Wi-Fi or by OS firewalls.

## Security notes

`chuanshu` is trustless about *integrity* (every piece is hash-verified) but not
about *privacy*: on a LAN, anyone running the tool can discover which info hashes
are circulating and pull any piece of them. Do not use it on a network you do not
trust to find out the names/sizes of the files you share.

## Development

```sh
go test ./...            # unit + integration tests
go test -race ./...      # with the race detector
go vet ./...
gofmt -l .
```

Layout:

```
cmd/chuanshu/        CLI (share / get / ls / hash)
internal/protocol/   metadata, info hash, bitfield, wire framing
internal/store/      piece hashing, verified reads/writes, resume
internal/discovery/  UDP multicast peer discovery + registry
internal/swarm/      node: TCP piece server, download loop, seeding
internal/web/        optional local web UI (loopback HTTP control plane)
```

## License

MIT © 2026 cycleuser
