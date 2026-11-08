package swarm

import "net"

// localAddr returns a best-effort local IPv4 address by "dialing" a public
// address without sending traffic (a standard Go trick to learn the preferred
// outbound interface address). Falls back to 127.0.0.1.
func localAddr() (string, error) {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return "127.0.0.1", err
	}
	defer conn.Close()
	if ua, ok := conn.LocalAddr().(*net.UDPAddr); ok {
		return ua.IP.String(), nil
	}
	return "127.0.0.1", nil
}
