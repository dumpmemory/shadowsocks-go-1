//go:build !linux && !windows && !darwin && !freebsd

package conn

import (
	"context"
	"net"
)

// ListenUDP wraps [net.ListenConfig.ListenPacket] and sets socket options on supported platforms.
//
// On Linux and Windows, IP_MTU_DISCOVER and IPV6_MTU_DISCOVER are set to IP_PMTUDISC_DO to disable IP fragmentation
// and encourage correct MTU settings. If pktinfo is true, IP_PKTINFO and IPV6_RECVPKTINFO are set to 1.
//
// On Linux, SO_MARK is set to user-specified value.
//
// On macOS and FreeBSD, IP_DONTFRAG, IPV6_DONTFRAG are set to 1 (Don't Fragment).
func ListenUDP(network string, laddr string, pktinfo bool, fwmark int) (*net.UDPConn, error) {
	var lc net.ListenConfig
	pc, err := lc.ListenPacket(context.Background(), network, laddr)
	if err != nil {
		return nil, err
	}
	return pc.(*net.UDPConn), nil
}
