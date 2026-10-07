package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	plexprefs "github.com/mediactl/clusterplex/pkg/plex/prefs"
	"github.com/mediactl/clusterplex/pkg/plex/servertoken"
)

func tokenManager(t *testing.T, token string) *Manager {
	t.Helper()
	m := newTestManager()
	m.Config.PlexDir = t.TempDir()
	m.Config.Clustarr = ClustarrConfig{Enabled: true, Namespace: "clustarr-system", TokenSecret: "plex-token"}
	content := `<Preferences ProcessedMachineIdentifier="server-id"`
	if token != "" {
		content += ` PlexOnlineToken="` + token + `"`
	}
	require.NoError(t, os.WriteFile(filepath.Join(m.Config.PlexDir, "Preferences.xml"), []byte(content+`/>`), 0o600))
	return m
}

func secretToken(t *testing.T, m *Manager) string {
	t.Helper()
	sec, err := m.K8sClient.CoreV1().Secrets("clustarr-system").Get(context.Background(), "plex-token", metav1.GetOptions{})
	if err != nil {
		return ""
	}
	return string(sec.Data[servertoken.TokenKey])
}

// Before Plex starts, a claim code is exchanged for the server's token and
// recorded in the Secret clustarr reads.
func TestAClaimIsExchangedBeforePlexStarts(t *testing.T) {
	m := tokenManager(t, "")
	m.Config.Claim = "claim-abc"
	m.claimExchange = func(_ context.Context, claim, clientID string) (string, error) {
		assert.Equal(t, "claim-abc", claim)
		assert.Equal(t, "server-id", clientID)
		return "server-token", nil
	}
	require.NoError(t, m.settleServerToken(t.Context()))

	got, err := plexprefs.Value(m.Config.PreferencesFile(), "PlexOnlineToken")
	require.NoError(t, err)
	assert.Equal(t, "server-token", got)
	assert.Equal(t, "server-token", secretToken(t, m))
}

// Without a token Secret the manager touches no Secret at all.
func TestNoTokenSecretWritesNoSecret(t *testing.T) {
	m := tokenManager(t, "server-token")
	m.Config.Clustarr.TokenSecret = ""
	require.NoError(t, m.settleServerToken(t.Context()))
	m.tokenSyncEvery = 10 * time.Millisecond
	m.takePlexTV(t.Context())
	defer m.stopLeaderWork()
	time.Sleep(50 * time.Millisecond)
	list, err := m.K8sClient.CoreV1().Secrets("clustarr-system").List(t.Context(), metav1.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, list.Items)
}

// The first lease holder writes the file's account into clustarr's absent
// Secret; from then on the Secret is the master, so a sign-in through Plex
// Web that lands in the file is put back, and the next start of every pod
// is held to the Secret's account too.
func TestTheLeaseHolderWritesTheSecretOnceThenHoldsTheFileToIt(t *testing.T) {
	m := tokenManager(t, "server-token")
	_, err := plexprefs.Apply(m.Config.PreferencesFile(), map[string]string{"PlexOnlineUsername": "appkins", "PlexOnlineMail": "me@example.com"})
	require.NoError(t, err)
	m.tokenSyncEvery = 10 * time.Millisecond
	m.takePlexTV(t.Context())
	defer m.stopLeaderWork()
	require.Eventually(t, func() bool { return secretToken(t, m) == "server-token" }, 5*time.Second, 10*time.Millisecond)
	sec, err := m.K8sClient.CoreV1().Secrets("clustarr-system").Get(t.Context(), "plex-token", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "appkins", string(sec.Data[servertoken.UsernameKey]))
	assert.Equal(t, "me@example.com", string(sec.Data[servertoken.EmailKey]))

	_, err = plexprefs.Apply(m.Config.PreferencesFile(), map[string]string{"PlexOnlineToken": "signed-in-again", "PlexOnlineUsername": "someone-else"})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		tok, _ := plexprefs.Value(m.Config.PreferencesFile(), "PlexOnlineToken")
		user, _ := plexprefs.Value(m.Config.PreferencesFile(), "PlexOnlineUsername")
		return tok == "server-token" && user == "appkins"
	}, 5*time.Second, 10*time.Millisecond, "the Secret's account is put back")
	assert.Equal(t, "server-token", secretToken(t, m), "the Secret does not follow the file")
}
