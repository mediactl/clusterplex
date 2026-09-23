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
