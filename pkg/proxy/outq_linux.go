//go:build linux

package proxy

import (
	"net"
	"syscall"
	"unsafe"
)

// sendQueue reports how many bytes written to c the kernel has not yet had
// acknowledged by the peer: what is still on this side of the wire.
func sendQueue(c net.Conn) (int, bool) {
	tc, ok := c.(*net.TCPConn)
	if !ok {
		return 0, false
	}
	raw, err := tc.SyscallConn()
	if err != nil {
		return 0, false
	}
	var pending int32
	var ioctlErr syscall.Errno
	if err := raw.Control(func(fd uintptr) {
		_, _, ioctlErr = syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TIOCOUTQ, uintptr(unsafe.Pointer(&pending)))
	}); err != nil || ioctlErr != 0 {
		return 0, false
	}
	return int(pending), true
}

// socketError reports the pending error on c, such as the reset a client
// sends when it closes with data unread, or 0 when there is none.
func socketError(c net.Conn) (int, bool) {
	tc, ok := c.(*net.TCPConn)
	if !ok {
		return 0, false
	}
	raw, err := tc.SyscallConn()
	if err != nil {
		return 0, false
	}
	var soErr int
	var getErr error
	if err := raw.Control(func(fd uintptr) {
		soErr, getErr = syscall.GetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_ERROR)
	}); err != nil || getErr != nil {
		return 0, false
	}
	return soErr, true
}

// tcpClosed reports whether c's socket has reached TCP_CLOSE: what a reset
// from the peer leaves behind, with the unsent bytes still counted against
// it and the error already consumed by whoever last read from it.
func tcpClosed(c net.Conn) bool {
	tc, ok := c.(*net.TCPConn)
	if !ok {
		return false
	}
	raw, err := tc.SyscallConn()
	if err != nil {
		return false
	}
	var info syscall.TCPInfo
	length := uint32(unsafe.Sizeof(info))
	var errno syscall.Errno
	if err := raw.Control(func(fd uintptr) {
		_, _, errno = syscall.Syscall6(syscall.SYS_GETSOCKOPT, fd, syscall.IPPROTO_TCP, syscall.TCP_INFO,
			uintptr(unsafe.Pointer(&info)), uintptr(unsafe.Pointer(&length)), 0)
	}); err != nil || errno != 0 {
		return false
	}
	const tcpClose = 7
	return info.State == tcpClose
}
