//go:build (!unix && !windows) || solaris

// Multicast fallback for platforms without a raw socket API (plan9, js/wasm,
// solaris). Discovery cannot bind/join a multicast group there, so we surface
// a clear error at Start() instead of a confusing hang. The TCP data path
// still works if the caller learns peer addresses by some other means.

package discovery

import (
	"errors"
	"net"
	"syscall"
)

// ErrNotSupported indicates multicast discovery is not implemented on this
// platform.
var ErrNotSupported = errors.New("chuanshu: multicast discovery not supported on this platform")

func setMulticastSocketOptions(network, address string, c syscall.RawConn) error {
	return nil
}

func joinMulticast(conn *net.UDPConn) error {
	return ErrNotSupported
}
