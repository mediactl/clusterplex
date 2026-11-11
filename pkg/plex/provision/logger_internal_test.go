package provision

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// A type nobody handed a logger refuses to work rather than invent one
// (the 2026-11-10 structured-logging design §5): its lines would carry no
// service and no subsystem.
func TestANilLoggerIsRefused(t *testing.T) {
	_, err := (&Provisioner{}).Run(t.Context())
	assert.ErrorIs(t, err, errNoLogger)
}
