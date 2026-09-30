package provision

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestProviderRootsAreFetchedWithATimeout: a provider root that never
// answers must not hold the provisioner for ever.
func TestProviderRootsAreFetchedWithATimeout(t *testing.T) {
	require.NotZero(t, defaultHTTP.Timeout)
}
