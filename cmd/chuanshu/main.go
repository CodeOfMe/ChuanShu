// Command chuanshu (传书) is a zero-config LAN tool for sharing large files
// peer-to-peer. Start it on the machine that has the file, and everyone else on
// the same local network can fetch it directly from each other. Each downloader
// becomes a seeder, so the more people who pull a file, the faster it spreads.
package main

import (
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/CodeOfMe/ChuanShu/internal/swarm"
)

const (
	version = "0.1.0"
	// findTimeout bounds how long "get" waits for a seeder to appear.
	findTimeout = 120 * time.Second
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "share":
		err = cmdShare(os.Args[2:])
	case "get":
		err = cmdGet(os.Args[2:])
	case "ls":
		err = cmdLs(os.Args[2:])
	case "hash":
		err = cmdHash(os.Args[2:])
	case "version", "-v", "--version":
		fmt.Printf("chuanshu %s\n", version)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Print(`chuanshu 传书 - LAN P2P file transfer

Usage:
  chuanshu share <file> [--piece KiB]      Share a file and seed it
  chuanshu get   <hash> [--out DIR]        Fetch a file by its info hash
  chuanshu ls    [--wait S]                List files shared on the LAN
  chuanshu hash  <file> [--piece KiB]      Print a file's info hash
  chuanshu version                         Show version

A file is identified by its info hash. Run "share" on the machine that has the
file; everyone else runs "get <hash>" (or "ls" to browse). Anyone with a
complete copy automatically seeds it to the rest of the swarm.
`)
}

// buildNode creates and starts a swarm node. The TCP data port is chosen by the
// OS (ephemeral) so any number of instances can run on one host; peer discovery
// uses a shared multicast port.
func buildNode() (*swarm.Node, error) {
	n, err := swarm.NewNode(swarm.Options{})
	if err != nil {
		return nil, err
	}
	if err := n.Start(); err != nil {
		return nil, err
	}
	return n, nil
}

// reorderFlags lets flags appear in any position (e.g. `get <hash> --out dir`).
// It moves positional (non-flag) arguments to the end while keeping each flag
// together with the value it consumes.
func reorderFlags(fs *flag.FlagSet, args []string) []string {
	var flags, positional []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if len(a) < 2 || a[0] != '-' {
			positional = append(positional, a)
			continue
		}
		// A flag token. Keep it together with its value when the flag needs one
		// and the value is the following token (e.g. --out dir).
		flags = append(flags, a)
		name := a
		for len(name) > 0 && name[0] == '-' {
			name = name[1:]
		}
		if eq := strings.IndexByte(name, '='); eq >= 0 {
			continue // --flag=value, value already attached
		}
		if f := fs.Lookup(name); f != nil && !isBoolFlag(f.Value) {
			if i+1 < len(args) {
				flags = append(flags, args[i+1])
				i++
			}
		}
	}
	return append(flags, positional...)
}

// isBoolFlag reports whether a flag value is boolean (takes no separate value).
func isBoolFlag(v flag.Value) bool {
	bf, ok := v.(interface{ IsBoolFlag() bool })
	return ok && bf.IsBoolFlag()
}

func cmdShare(args []string) error {
	fs := flag.NewFlagSet("share", flag.ExitOnError)
	pieceKiB := fs.Int("piece", 256, "piece size in KiB")
	fs.Parse(reorderFlags(fs, args))

	if fs.NArg() < 1 {
		return fmt.Errorf("share requires a file path")
	}
	path := fs.Arg(0)
	n, err := buildNode()
	if err != nil {
		return err
	}
	defer n.Stop()

	meta, err := n.AddSeed(path, *pieceKiB*1024)
	if err != nil {
		return err
	}

	fmt.Printf("Sharing %s (%s)\n", filepath.Base(path), humanBytes(meta.Length))
	fmt.Printf("  info hash : %s\n", meta.InfoHashHex())
	fmt.Printf("  pieces    : %d x %s\n", meta.NumPieces(), humanBytes(int64(meta.PieceLength)))
	fmt.Printf("  from      : %s\n\n", n.MyAddr())
	fmt.Println("Others on this LAN can download it with:")
	fmt.Printf("    chuanshu get %s\n\n", meta.InfoHashHex())
	fmt.Println("Press Ctrl-C to stop sharing.")

	<-waitForSignal()
	fmt.Println("\nStopping.")
	return nil
}

func cmdGet(args []string) error {
	fs := flag.NewFlagSet("get", flag.ExitOnError)
	out := fs.String("out", ".", "output directory")
	fs.Parse(reorderFlags(fs, args))

	if fs.NArg() < 1 {
		return fmt.Errorf("get requires an info hash")
	}
	infoHex := fs.Arg(0)
	raw, err := hex.DecodeString(infoHex)
	if err != nil || len(raw) != 32 {
		return fmt.Errorf("invalid info hash (want 32-byte hex)")
	}
	var ih [32]byte
	copy(ih[:], raw)

	n, err := buildNode()
	if err != nil {
		return err
	}
	defer n.Stop()

	fmt.Printf("Looking for %s on the local network...\n", infoHex)

	deadline := time.Now().Add(findTimeout)
	sig := make(chan struct{})
	go func() {
		<-waitForSignal()
		close(sig)
	}()

	for {
		if addr := n.FindPeer(infoHex); addr != "" {
			m, err := n.GetMeta(addr, ih)
			if err == nil {
				fmt.Printf("Found seeder %s: %s (%s, %d pieces)\n", addr, m.Name, humanBytes(m.Length), m.NumPieces())
				if err := n.StartDownload(*out, m); err != nil {
					return err
				}
				break
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("no seeder advertising %s found within %s", infoHex, findTimeout)
		}
		select {
		case <-sig:
			fmt.Println("\nCancelled.")
			return nil
		case <-time.After(500 * time.Millisecond):
		}
	}

	fmt.Printf("Downloading into %s\n\n", *out)
	renderProgress(n, infoHex, sig)

	st, _ := n.StatusFor(infoHex)
	if st.Complete {
		target := filepath.Join(*out, st.Name)
		fmt.Printf("\nDone: %s\n", target)
		fmt.Println("Press Ctrl-C to stop seeding (or keep this running to help others).")
		<-waitForSignal()
		fmt.Println("\nStopping.")
		return nil
	}
	n.AbortDownload(infoHex)
	return fmt.Errorf("download did not complete")
}

func cmdLs(args []string) error {
	fs := flag.NewFlagSet("ls", flag.ExitOnError)
	waitSec := fs.Int("wait", 4, "seconds to listen for peers")
	fs.Parse(reorderFlags(fs, args))

	n, err := buildNode()
	if err != nil {
		return err
	}
	defer n.Stop()

	fmt.Printf("Listening on the local network for %ds...\n", *waitSec)
	time.Sleep(time.Duration(*waitSec) * time.Second)

	hashes := n.DiscoveredHashes()
	if len(hashes) == 0 {
		fmt.Println("No shared files found on the LAN yet.")
		return nil
	}
	fmt.Printf("%d file(s) shared on the LAN:\n\n", len(hashes))

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "SEEDERS\tSIZE\tNAME\tINFO HASH")
	sort.Strings(hashes)
	for _, infoHex := range hashes {
		seeds := n.SeederCount(infoHex)
		name := "-"
		size := "-"
		if addr := n.FindPeer(infoHex); addr != "" {
			var ih [32]byte
			copy(ih[:], mustHex(infoHex))
			if m, err := n.GetMeta(addr, ih); err == nil {
				name = m.Name
				size = humanBytes(m.Length)
			}
		}
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\n", seeds, size, name, infoHex)
	}
	return w.Flush()
}

func cmdHash(args []string) error {
	fs := flag.NewFlagSet("hash", flag.ExitOnError)
	pieceKiB := fs.Int("piece", 256, "piece size in KiB")
	fs.Parse(reorderFlags(fs, args))
	if fs.NArg() < 1 {
		return fmt.Errorf("hash requires a file path")
	}
	meta, _, err := buildMeta(fs.Arg(0), *pieceKiB*1024)
	if err != nil {
		return err
	}
	fmt.Println(meta.InfoHashHex())
	return nil
}

// renderProgress polls the node for status and prints a progress line.
func renderProgress(n *swarm.Node, infoHex string, stop <-chan struct{}) {
	t := time.NewTicker(400 * time.Millisecond)
	defer t.Stop()
	lastPct := -1.0
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			st, ok := n.StatusFor(infoHex)
			if !ok {
				return
			}
			pct := st.Progress * 100
			if pct != lastPct {
				fmt.Printf("  %-6.2f%%  (%d/%d pieces, %d peers, down %s)\r",
					pct, st.HavePieces, st.Pieces, st.Peers, humanBytes(st.Downloaded))
				lastPct = pct
			}
			if st.Complete {
				fmt.Println()
				return
			}
		}
	}
}

// waitForSignal returns a channel closed on the next SIGINT/SIGTERM.
func waitForSignal() <-chan struct{} {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	out := make(chan struct{})
	go func() {
		<-ch
		signal.Stop(ch)
		close(out)
	}()
	return out
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func mustHex(s string) []byte {
	raw, _ := hex.DecodeString(s)
	return raw
}
