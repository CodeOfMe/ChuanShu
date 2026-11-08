//go:build windows

// Windows multicast support. Go's stdlib does not expose IP_ADD_MEMBERSHIP on
// Windows and there is no SO_REUSEPORT equivalent, so we call the raw Windows
// socket API through golang.org/x/sys/windows. Multi-instance sharing of the
// discovery UDP port is not possible on Windows; each additional `chuanshu`
// instance on the same host will fail to bind the discovery port.

package discovery

import (
	"net"
	"syscall"

	"golang.org/x/sys/windows"
)

// setMulticastSocketOptions configures outbound multicast defaults. Windows
// has no SO_REUSEPORT, so we skip that option.
func setMulticastSocketOptions(network, address string, c syscall.RawConn) error {
	var serr error
	err := c.Control(func(fd uintptr) {
		h := windows.Handle(fd)
		// IP_MULTICAST_TTL = 22, IP_MULTICAST_LOOP = 20, IPPROTO_IP = 0
		_ = windows.SetsockoptInt(h, 0, 22, 2)
		_ = windows.SetsockoptInt(h, 0, 20, 1)
	})
	if err != nil {
		return err
	}
	return serr
}

// joinMulticast joins the discovery multicast group on every up,
// multicast-capable interface via the raw IP_ADD_MEMBERSHIP setsockopt.
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
		h := windows.Handle(fd)
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
				mreq := windows.IPMreq{
					Multiaddr: [4]byte{multicastGroup[12], multicastGroup[13], multicastGroup[14], multicastGroup[15]},
					Interface: [4]byte{ip4[0], ip4[1], ip4[2], ip4[3]},
				}
				// IP_ADD_MEMBERSHIP = 12, IPPROTO_IP = 0
				if e := windows.SetsockoptIPMreq(h, 0, 12, &mreq); e != nil {
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
