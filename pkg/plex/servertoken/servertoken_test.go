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

// accountFile is a Preferences.xml signed in as an account.
func accountFile(t *testing.T, token, username, mail string) string {
	t.Helper()
	p := prefsFile(t, token)
	_, err := plexprefs.Apply(p, map[string]string{"PlexOnlineUsername": username, "PlexOnlineMail": mail})
	require.NoError(t, err)
	return p
}

// accountOf is the account the file holds.
func accountOf(t *testing.T, p string) [3]string {
	t.Helper()
	var out [3]string
	for i, pref := range []string{"PlexOnlineToken", "PlexOnlineUsername", "PlexOnlineMail"} {
		v, err := plexprefs.Value(p, pref)
		require.NoError(t, err)
		out[i] = v
	}
	return out
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

// The Secret is the master copy once it holds a token: every pod starts
// with its account, whatever the file says -- a sign-in through Plex Web on
// one replica does not move the others.
func TestTheSecretsAccountIsForcedBeforePlexStarts(t *testing.T) {
	p := accountFile(t, "newer", "someone-else", "else@example.com")
	store := &memStore{stored: Stored{Token: "older", Username: "appkins", Email: "me@example.com"}}
	require.NoError(t, starter(p, "", store, &plexTV{}).Run(context.Background()))
	assert.Equal(t, [3]string{"older", "appkins", "me@example.com"}, accountOf(t, p))
	assert.Zero(t, store.puts)
}

// An empty field is never forced: a Secret without a username leaves the
// file's.
func TestAnEmptyFieldIsNeverForced(t *testing.T) {
	p := accountFile(t, "older", "appkins", "me@example.com")
	store := &memStore{stored: Stored{Token: "older"}}
	require.NoError(t, starter(p, "", store, &plexTV{}).Run(context.Background()))
	assert.Equal(t, [3]string{"older", "appkins", "me@example.com"}, accountOf(t, p))
}

// Without a Secret, or with one that holds no token, a start forces
// nothing: the lease holder writes the file's account into it first.
func TestAStartWithoutASecretTokenLeavesTheFileAlone(t *testing.T) {
	p := accountFile(t, "server-token", "appkins", "me@example.com")
	store := &memStore{stored: Stored{Username: "stale"}}
	require.NoError(t, starter(p, "", store, &plexTV{}).Run(context.Background()))
	assert.Equal(t, [3]string{"server-token", "appkins", "me@example.com"}, accountOf(t, p))
}

// A new claim code may sign in another account: the fresh token replaces
// the old in both, and the old account's username and email go from both,
// to be filled from the file once Plex has written the new ones.
func TestANewClaimResetsTheAccountItReplaces(t *testing.T) {
	p := accountFile(t, "old-token", "appkins", "me@example.com")
	store := &memStore{stored: Stored{Token: "old-token", Username: "appkins", Email: "me@example.com", ClaimHash: Hash("claim-1")}}
	tv := &plexTV{token: "fresh"}
	require.NoError(t, starter(p, "claim-2", store, tv).Run(context.Background()))
	assert.Equal(t, 1, tv.calls)
	assert.Equal(t, [3]string{"fresh", "", ""}, accountOf(t, p))
	assert.Equal(t, Stored{Token: "fresh", ClaimHash: Hash("claim-2")}, store.stored)
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

func keeper(p string, store Store) Keeper { return Keeper{Prefs: p, Store: store, Logger: quiet()} }

// With no Secret token the lease holder writes the file's whole account
// into it, keeping the claim record; then the two agree and nothing is
// written again.
func TestTheFirstLeaseHolderWritesTheFilesAccountIntoTheSecret(t *testing.T) {
	p := accountFile(t, "server-token", "appkins", "me@example.com")
	store := &memStore{stored: Stored{ClaimHash: Hash("claim-1")}}
	require.NoError(t, keeper(p, store).Sync(context.Background()))
	assert.Equal(t, Stored{Token: "server-token", Username: "appkins", Email: "me@example.com", ClaimHash: Hash("claim-1")}, store.stored)

	require.NoError(t, keeper(p, store).Sync(context.Background()))
	assert.Equal(t, 1, store.puts, "an account that agrees is not written again")
}

func TestTheKeeperLeavesAnEmptySecretAloneWithoutAToken(t *testing.T) {
	p := prefsFile(t, "")
	store := &memStore{}
	require.NoError(t, keeper(p, store).Sync(context.Background()))
	assert.Zero(t, store.puts)
}

// Once the Secret holds a token it is the master: a file that drifted --
// a sign-in through Plex Web -- gets the Secret's account back, and the
// Secret is not touched.
func TestTheKeeperPutsTheSecretsAccountBackIntoAFileThatDrifted(t *testing.T) {
	p := accountFile(t, "signed-in-again", "someone-else", "else@example.com")
	store := &memStore{stored: Stored{Token: "server-token", Username: "appkins", Email: "me@example.com"}}
	require.NoError(t, keeper(p, store).Sync(context.Background()))
	assert.Equal(t, [3]string{"server-token", "appkins", "me@example.com"}, accountOf(t, p))
	assert.Zero(t, store.puts)

	lost := prefsFile(t, "")
	require.NoError(t, keeper(lost, store).Sync(context.Background()))
	assert.Equal(t, [3]string{"server-token", "appkins", "me@example.com"}, accountOf(t, lost), "a lost token is restored")
}

// A field the Secret lacks -- a Secret written before it held the
// username and email, or after a claim -- is filled from the file.
func TestTheKeeperFillsAFieldTheSecretLacks(t *testing.T) {
	p := accountFile(t, "server-token", "appkins", "me@example.com")
	store := &memStore{stored: Stored{Token: "server-token", ClaimHash: Hash("claim-1")}}
	require.NoError(t, keeper(p, store).Sync(context.Background()))
	assert.Equal(t, Stored{Token: "server-token", Username: "appkins", Email: "me@example.com", ClaimHash: Hash("claim-1")}, store.stored)
	assert.Equal(t, [3]string{"server-token", "appkins", "me@example.com"}, accountOf(t, p))
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

	require.NoError(t, s.Put(ctx, Stored{Token: "t2", Username: "appkins", Email: "me@example.com", ClaimHash: "h1"}))
	got, err = s.Get(ctx)
	require.NoError(t, err)
	assert.Equal(t, Stored{Token: "t2", Username: "appkins", Email: "me@example.com", ClaimHash: "h1"}, got)
	sec, err = cs.CoreV1().Secrets("clustarr-system").Get(ctx, "plex-token", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "appkins", string(sec.Data[UsernameKey]))
	assert.Equal(t, "me@example.com", string(sec.Data[EmailKey]))

	require.NoError(t, s.Put(ctx, Stored{Token: "t3", ClaimHash: "h2"}))
	got, err = s.Get(ctx)
	require.NoError(t, err)
	assert.Equal(t, Stored{Token: "t3", ClaimHash: "h2"}, got, "an empty field removes its key")
	sec, err = cs.CoreV1().Secrets("clustarr-system").Get(ctx, "plex-token", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "someone-elses", string(sec.Data["clientID"]))
	assert.Equal(t, corev1.SecretTypeOpaque, sec.Type)
}

func TestHashIsNotTheClaim(t *testing.T) {
	assert.NotContains(t, Hash("claim-1"), "claim-1")
	assert.Len(t, Hash("claim-1"), 64)
}
