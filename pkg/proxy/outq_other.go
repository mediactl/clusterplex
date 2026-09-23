//go:build !linux

package proxy

import "net"

// sendQueue is unknown off Linux; the connection is treated as delivered.
func sendQueue(net.Conn) (int, bool) { return 0, false }

// socketError is unknown off Linux.
func socketError(net.Conn) (int, bool) { return 0, false }

// tcpClosed is unknown off Linux.
func tcpClosed(net.Conn) bool { return false }
