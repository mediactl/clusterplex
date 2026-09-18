package portredirect

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeRunner struct {
	calls [][]string
	errs  map[string]error // keyed by the iptables operation flag, "-C" or "-A"
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) error {
	f.calls = append(f.calls, append([]string{name}, args...))
	for _, a := range args {
		if err, ok := f.errs[a]; ok {
			return err
		}
	}
	return nil
}

var (
	wantCheck  = []string{"iptables", "-w", "5", "-t", "nat", "-C", "PREROUTING", "-p", "tcp", "--dport", "32400", "-j", "REDIRECT", "--to-ports", "32401"}
	wantAppend = []string{"iptables", "-w", "5", "-t", "nat", "-A", "PREROUTING", "-p", "tcp", "--dport", "32400", "-j", "REDIRECT", "--to-ports", "32401"}
)

func TestEnsureAppendsRuleWhenMissing(t *testing.T) {
	r := &fakeRunner{errs: map[string]error{"-C": errors.New("iptables: Bad rule")}}
	require.NoError(t, Ensure(context.Background(), r, 32400, 32401))
	require.Len(t, r.calls, 2)
	assert.Equal(t, wantCheck, r.calls[0])
	assert.Equal(t, wantAppend, r.calls[1])
}

func TestEnsureIsIdempotentWhenRulePresent(t *testing.T) {
	r := &fakeRunner{}
	require.NoError(t, Ensure(context.Background(), r, 32400, 32401))
	require.Len(t, r.calls, 1)
	assert.Equal(t, wantCheck, r.calls[0])
}

func TestEnsureReturnsAppendFailure(t *testing.T) {
	r := &fakeRunner{errs: map[string]error{"-C": errors.New("missing"), "-A": errors.New("permission denied")}}
	err := Ensure(context.Background(), r, 32400, 32401)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "permission denied")
}
