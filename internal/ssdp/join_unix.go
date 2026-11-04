//go:build linux || darwin

package ssdp

import (
	"net"
	"syscall"

	"github.com/ArrowTech-Labs/spoken-parts-link/internal/logx"
)

// joinAll joins the group on every up, multicast capable IPv4 interface.
// ListenMulticastUDP with a nil interface joins only on the default one.
func joinAll(conn *net.UDPConn, group net.IP, log *logx.Logger) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return
	}
	raw, err := conn.SyscallConn()
	if err != nil {
		return
	}
	g4 := group.To4()
	for _, ifi := range ifaces {
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagMulticast == 0 || ifi.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifi.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok || ipn.IP.To4() == nil {
				continue
			}
			mreq := &syscall.IPMreq{}
			copy(mreq.Multiaddr[:], g4)
			copy(mreq.Interface[:], ipn.IP.To4())
			var serr error
			_ = raw.Control(func(fd uintptr) {
				serr = syscall.SetsockoptIPMreq(int(fd), syscall.IPPROTO_IP, syscall.IP_ADD_MEMBERSHIP, mreq)
			})
			// EADDRINUSE means this interface already joined. Anything else
			// is only worth a debug line.
			if serr != nil && serr != syscall.EADDRINUSE {
				log.Debug("discovery group join skipped", "interface", ifi.Name, "err", serr)
			}
			break
		}
	}
}
