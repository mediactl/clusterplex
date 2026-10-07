//go:build !linux

package livetv

import "errors"

// EnsureLinkLocal needs Linux's netlink.
func EnsureLinkLocal(string) error {
	return errors.New("livetv: the link-local address needs Linux")
}
