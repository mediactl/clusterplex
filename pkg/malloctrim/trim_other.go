//go:build !linux

package malloctrim

// Trim has nothing to call off Linux.
func Trim() (bool, error) { return false, ErrUnsupported }
