//go:build !linux

package proxy

import "net"

// sendQueue is unknown off Linux; the connection is treated as delivered.
func sendQueue(net.Conn) (int, bool) { return 0, false }
