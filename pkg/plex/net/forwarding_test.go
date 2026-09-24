package net

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestForwardingAlreadyOnIsAcceptedEvenWhenTheSwitchIsReadOnly(t *testing.T) {
	// An unprivileged pod has /proc/sys read-only, and Kubernetes has set
	// net.ipv4.ip_forward=1 in the pod namespace before any container ran,
	// when the pod asked for it. Forwarding is on; insisting on writing it
	// failed the whole namespace for nothing.
	path := filepath.Join(t.TempDir(), "ip_forward")
	require.NoError(t, os.WriteFile(path, []byte("1\n"), 0o444))
	assert.NoError(t, enableForwardingAt(path))
}

func TestForwardingOffIsTurnedOn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ip_forward")
	require.NoError(t, os.WriteFile(path, []byte("0\n"), 0o644))
	require.NoError(t, enableForwardingAt(path))
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "1\n", string(got))
}

func TestForwardingOffAndUnwritableIsAnErrorThatSaysWhatToDo(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write a read-only file")
	}
	path := filepath.Join(t.TempDir(), "ip_forward")
	require.NoError(t, os.WriteFile(path, []byte("0\n"), 0o444))
	err := enableForwardingAt(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "securityContext.sysctls")
}
