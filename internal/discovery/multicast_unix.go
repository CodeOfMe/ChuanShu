//go:build unix && !solaris

// Platform-specific multicast socket tuning for BSD/Linux/Darwin. Uses raw
// setsockopt calls because Go's stdlib API cannot express SO_REUSEPORT and
// does not let a single socket join the same group on every interface
// (macOS rejects the second join when both specify 0.0.0.0 as the interface).
// Solaris is excluded because it has no SO_REUSEPORT at all.

package discovery

import (
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

// setMulticastSocketOptions configures SO_REUSEPORT and outbound multicast
// settings on the raw socket before it is used by the net poller.
func setMulticastSocketOptions(network, address string, c syscall.RawConn) error {
	var serr error
	err := c.Control(func(fd uintptr) {
		serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
		if serr != nil {
			return
		}
		_ = unix.SetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_MULTICAST_TTL, 2)
		_ = unix.SetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_MULTICAST_LOOP, 1)
	})
	if err != nil {
		return err
	}
	return serr
}

// joinMulticast joins the discovery multicast group on every up, multicast
// capable interface so peers reachable via any NIC are heard. Each join is
// keyed by that interface's own IPv4 address so the (group, interface) pairs
// stay distinct; a join is best-effort and only fatal if none succeed.
func joinMulticast(conn *net.UDPConn) error {
	rc, err := conn.SyscallConn()
	if err != nil {
		return err
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		return err
	}
	var anyJoined bool
	var lastErr error
	err = rc.Control(func(fd uintptr) {
		for _, ifc := range ifaces {
			if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagMulticast == 0 {
				continue
			}
			addrs, err := ifc.Addrs()
			if err != nil {
				continue
			}
			for _, a := range addrs {
				ipn, ok := a.(*net.IPNet)
				if !ok {
					continue
				}
				ip4 := ipn.IP.To4()
				if ip4 == nil {
					continue
				}
				mreq := unix.IPMreq{
					Multiaddr: [4]byte{multicastGroup[12], multicastGroup[13], multicastGroup[14], multicastGroup[15]},
					Interface: [4]byte{ip4[0], ip4[1], ip4[2], ip4[3]},
				}
				if e := unix.SetsockoptIPMreq(int(fd), unix.IPPROTO_IP, unix.IP_ADD_MEMBERSHIP, &mreq); e != nil {
					lastErr = e
					continue
				}
				anyJoined = true
			}
		}
	})
	if err != nil {
		return err
	}
	if !anyJoined && lastErr != nil {
		return lastErr
	}
	return nil
}
