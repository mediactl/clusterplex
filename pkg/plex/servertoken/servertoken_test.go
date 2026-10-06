package servertoken

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	plexprefs "github.com/mediactl/clusterplex/pkg/plex/prefs"
)

const serverID = "25648e79229b88b46fdb829e0bdabedf7c385304"

func prefsFile(t *testing.T, token string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "Preferences.xml")
	content := `<Preferences MachineIdentifier="9c67996e-8b08-44b9-9c83-a6d317322a2d" ProcessedMachineIdentifier="` + serverID + `"`
	if token != "" {
		content += ` PlexOnlineToken="` + token + `"`
	}
	require.NoError(t, os.WriteFile(p, []byte(content+`/>`), 0o600))
	return p
}

func tokenIn(t *testing.T, p string) string {
	t.Helper()
	v, err := plexprefs.Value(p, "PlexOnlineToken")
	require.NoError(t, err)
	return v
}

// memStore is a Store in memory.
type memStore struct {
	mu     sync.Mutex
	stored Stored
	getErr error
	puts   int
}

func (m *memStore) Get(context.Context) (Stored, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stored, m.getErr
}

func (m *memStore) Put(_ context.Context, s Stored) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stored = s
	m.puts++
	return nil
}

// plexTV is a fake exchange counting its calls.
type plexTV struct {
	mu    sync.Mutex
	calls int
	token string
	err   error
	ids   []string
}

func (p *plexTV) exchange(_ context.Context, _, clientID string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	p.ids = append(p.ids, clientID)
	return p.token, p.err
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func starter(p string, claim string, store Store, tv *plexTV) Starter {
	s := Starter{Prefs: p, Claim: claim, Exchange: tv.exchange, Logger: quiet()}
	if store != nil {
		s.Store = store
	}
	return s
}

func TestAClaimOnAnUnclaimedServerIsExchangedAndRecorded(t *testing.T) {
	p := prefsFile(t, "")
	store := &memStore{}
	tv := &plexTV{token: "server-token"}
	require.NoError(t, starter(p, "claim-1", store, tv).Run(context.Background()))

	assert.Equal(t, "server-token", tokenIn(t, p))
	assert.Equal(t, []string{serverID}, tv.ids, "claimed as the server's processed identity")
	assert.Equal(t, Stored{Token: "server-token", ClaimHash: Hash("claim-1")}, store.stored)
}

// The claim code works once: the pod that spent it records it, and the pods
// starting after it, or the same pod restarting, leave it alone.
func TestAClaimAlreadySpentIsNotExchangedAgain(t *testing.T) {
	p := prefsFile(t, "server-token")
	store := &memStore{stored: Stored{Token: "server-token", ClaimHash: Hash("claim-1")}}
	tv := &plexTV{token: "other"}
	require.NoError(t, starter(p, "claim-1", store, tv).Run(context.Background()))
	assert.Zero(t, tv.calls)
	assert.Equal(t, "server-token", tokenIn(t, p))
}

// A re-claim: a new code replaces the token the file holds, revoked or not.
func TestANewClaimReplacesTheTokenTheFileHolds(t *testing.T) {
	p := prefsFile(t, "revoked")
	store := &memStore{stored: Stored{Token: "revoked", ClaimHash: Hash("claim-1")}}
	tv := &plexTV{token: "fresh"}
	require.NoError(t, starter(p, "claim-2", store, tv).Run(context.Background()))
	assert.Equal(t, "fresh", tokenIn(t, p))
	assert.Equal(t, Stored{Token: "fresh", ClaimHash: Hash("claim-2")}, store.stored)
}

// Without a Secret there is no record of a spent code, so a claim is
// exchanged only for a server with no token, as Plex's own image does.
func TestWithoutASecretAClaimIsExchangedOnlyWithoutAToken(t *testing.T) {
	p := prefsFile(t, "server-token")
	tv := &plexTV{token: "other"}
	require.NoError(t, starter(p, "claim-1", nil, tv).Run(context.Background()))
	assert.Zero(t, tv.calls)

	p = prefsFile(t, "")
	tv = &plexTV{token: "server-token"}
	require.NoError(t, starter(p, "claim-1", nil, tv).Run(context.Background()))
	assert.Equal(t, "server-token", tokenIn(t, p))
}

// A refused exchange -- an expired code -- keeps the token the file has and
// records nothing, so Plex starts as it would have.
func TestARefusedClaimKeepsTheExistingToken(t *testing.T) {
	p := prefsFile(t, "revoked")
	store := &memStore{stored: Stored{Token: "revoked"}}
	tv := &plexTV{err: errors.New("plex.tv refused the exchange: 401")}
	require.NoError(t, starter(p, "claim-2", store, tv).Run(context.Background()))
	assert.Equal(t, "revoked", tokenIn(t, p))
	assert.Zero(t, store.puts)
}

// A lost plex-config: the file has no token and no claim is given, so the
// Secret's copy goes back in.
func TestALostTokenIsRestoredFromTheSecret(t *testing.T) {
	p := prefsFile(t, "")
	store := &memStore{stored: Stored{Token: "server-token"}}
	require.NoError(t, starter(p, "", store, &plexTV{}).Run(context.Background()))
	assert.Equal(t, "server-token", tokenIn(t, p))
}

// The file is the master copy: a Secret holding another token never
// overwrites one the file has. A sign-in through Plex Web lands in the file,
// and the Secret catches up through the mirror.
func TestTheSecretNeverOverwritesATokenTheFileHas(t *testing.T) {
	p := prefsFile(t, "newer")
	store := &memStore{stored: Stored{Token: "older"}}
	require.NoError(t, starter(p, "", store, &plexTV{}).Run(context.Background()))
	assert.Equal(t, "newer", tokenIn(t, p))
}

// A Secret that cannot be read is no reason to keep Plex from starting.
func TestAnUnreadableSecretStillStartsPlex(t *testing.T) {
	p := prefsFile(t, "server-token")
	store := &memStore{getErr: errors.New("forbidden")}
	tv := &plexTV{token: "other"}
	require.NoError(t, starter(p, "claim-1", store, tv).Run(context.Background()))
	assert.Zero(t, tv.calls, "a spent code cannot be told from a new one without the Secret")
	assert.Equal(t, "server-token", tokenIn(t, p))
}

// Three pods starting together on one file with one code: exactly one spends
// it, because the decision runs under the preferences lock.
func TestPodsStartingTogetherSpendTheClaimOnce(t *testing.T) {
	p := prefsFile(t, "")
	store := &memStore{}
	tv := &plexTV{token: "server-token"}
	var wg sync.WaitGroup
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			assert.NoError(t, starter(p, "claim-1", store, tv).Run(context.Background()))
		}()
	}
	wg.Wait()
	assert.Equal(t, 1, tv.calls)
	assert.Equal(t, "server-token", tokenIn(t, p))
}

func TestTheMirrorCopiesTheFilesTokenIntoTheSecret(t *testing.T) {
	p := prefsFile(t, "server-token")
	store := &memStore{stored: Stored{Token: "older", ClaimHash: Hash("claim-1")}}
	require.NoError(t, Mirror{Prefs: p, Store: store}.Sync(context.Background()))
	assert.Equal(t, Stored{Token: "server-token", ClaimHash: Hash("claim-1")}, store.stored)

	require.NoError(t, Mirror{Prefs: p, Store: store}.Sync(context.Background()))
	assert.Equal(t, 1, store.puts, "an unchanged token is not written again")
}

func TestTheMirrorLeavesTheSecretAloneWithoutAToken(t *testing.T) {
	p := prefsFile(t, "")
	store := &memStore{stored: Stored{Token: "server-token"}}
	require.NoError(t, Mirror{Prefs: p, Store: store}.Sync(context.Background()))
	assert.Zero(t, store.puts)
}

func TestTheSecretIsCreatedThenUpdatedKeepingWhatElseItHolds(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewSimpleClientset()
	s := Secret{Client: cs, Namespace: "clustarr-system", Name: "plex-token"}

	got, err := s.Get(ctx)
	require.NoError(t, err)
	assert.Equal(t, Stored{}, got, "an absent Secret reads as empty")

	require.NoError(t, s.Put(ctx, Stored{Token: "t1", ClaimHash: "h1"}))
	sec, err := cs.CoreV1().Secrets("clustarr-system").Get(ctx, "plex-token", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "t1", string(sec.Data[TokenKey]))
	assert.Equal(t, "h1", sec.Annotations[ClaimAnnotation])

	sec.Data["clientID"] = []byte("someone-elses")
	_, err = cs.CoreV1().Secrets("clustarr-system").Update(ctx, sec, metav1.UpdateOptions{})
	require.NoError(t, err)

	require.NoError(t, s.Put(ctx, Stored{Token: "t2", ClaimHash: "h1"}))
	got, err = s.Get(ctx)
	require.NoError(t, err)
	assert.Equal(t, Stored{Token: "t2", ClaimHash: "h1"}, got)
	sec, err = cs.CoreV1().Secrets("clustarr-system").Get(ctx, "plex-token", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "someone-elses", string(sec.Data["clientID"]))
	assert.Equal(t, corev1.SecretTypeOpaque, sec.Type)
}

func TestHashIsNotTheClaim(t *testing.T) {
	assert.NotContains(t, Hash("claim-1"), "claim-1")
	assert.Len(t, Hash("claim-1"), 64)
}
