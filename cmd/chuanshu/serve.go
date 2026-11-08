package main

import (
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"

	"github.com/CodeOfMe/ChuanShu/internal/swarm"
	"github.com/CodeOfMe/ChuanShu/internal/web"
)

// cmdServe starts a swarm node plus a local web control panel (the optional
// graphical UI). The HTTP server binds loopback only; LAN peers keep talking to
// the node directly over the same UDP-multicast + TCP transport the CLI uses.
func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:0", "loopback address to serve the UI on (port 0 = auto)")
	noOpen := fs.Bool("no-open", false, "do not try to open a browser automatically")
	fs.Parse(reorderFlags(fs, args))

	host, _, err := net.SplitHostPort(*addr)
	if err != nil {
		return fmt.Errorf("invalid --addr %q: %w", *addr, err)
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("the UI must stay on loopback for safety, got --addr %q", *addr)
	}

	n, err := swarm.NewNode(swarm.Options{})
	if err != nil {
		return err
	}
	if err := n.Start(); err != nil {
		return err
	}
	defer n.Stop()

	srv, err := web.NewServer(n, *addr)
	if err != nil {
		return err
	}
	if err := srv.Start(); err != nil {
		return err
	}
	defer srv.Close()

	url := fmt.Sprintf("http://%s/", srv.Addr())
	fmt.Printf("传书 UI 已启动: %s\n", url)
	fmt.Printf("节点对外地址: %s\n", n.MyAddr())
	if !*noOpen {
		if oerr := openBrowser(url); oerr != nil {
			fmt.Printf("(无法自动打开浏览器,请手动访问 %s)\n", url)
		}
	}
	fmt.Println("保持此终端运行;按 Ctrl-C 退出。")
	<-waitForSignal()
	fmt.Println("\nStopping.")
	return nil
}

// openBrowser tries to launch the default browser at url. It is best-effort;
// the caller prints the URL regardless of the result.
func openBrowser(url string) error {
	var name string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		name = "open"
		args = []string{url}
	case "windows":
		name = "rundll32"
		args = []string{"url.dll,FileProtocolHandler", url}
	default: // linux & friends
		name = "xdg-open"
		args = []string{url}
	}
	cmd := exec.Command(name, args...)
	// Do not inherit our stdin/stdout/stderr so the launcher cannot block us.
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd.Start()
}
