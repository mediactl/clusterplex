// Package servertoken keeps the server's plex.tv account -- PlexOnlineToken,
// PlexOnlineUsername and PlexOnlineMail -- in a Secret clustarr reads (its
// Plex watchlist ImportList), and holds every replica to it.
//
// The Secret is the master copy once it holds a token (2026-10-07; until
// then Preferences.xml was, and the Secret only followed it). With no
// Secret, or one without a token, the lease holder writes the file's
// account into it -- one pod at a time, so the first lease holder's. After
// that every pod forces the Secret's values into Preferences.xml before
// Plex starts, so every replica starts signed in as the same account, and
// the lease holder puts them back whenever the file drifts, as a sign-in
// through Plex Web makes it. A Plex already running keeps the account it
// read until it restarts. An empty value is never forced: a field the
// Secret lacks is filled from the file instead.
//
// To change the account, edit or delete the Secret, or spend a new claim
// code. A code is exchanged before Plex starts, for a server with no token
// or, with a Secret, for any code other than the one it records as spent;
// the fresh token replaces the old in both, and the new account's username
// and email fill in from the file once Plex has written them.
package servertoken

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	plexprefs "github.com/mediactl/clusterplex/pkg/plex/prefs"
)

const (
	// TokenKey is the Secret key holding the token; clustarr's Plex
	// ImportList reads "token".
	TokenKey = "token"
	// UsernameKey and EmailKey hold the account's PlexOnlineUsername and
	// PlexOnlineMail.
	UsernameKey = "username"
	EmailKey    = "email"
	// ClaimAnnotation records the SHA-256 of the last claim code spent.
	ClaimAnnotation = "clusterplex.mediactl.io/claim-sha256"

	tokenPref    = "PlexOnlineToken"
	usernamePref = "PlexOnlineUsername"
	mailPref     = "PlexOnlineMail"
	clientIDPref = "ProcessedMachineIdentifier"

	// exchangeTimeout bounds the call to plex.tv, which runs while the
	// preferences lock keeps the other pods from starting Plex.
	exchangeTimeout = 30 * time.Second
)

// Hash is what ClaimAnnotation records for a claim code.
func Hash(claim string) string {
	h := sha256.Sum256([]byte(claim))
	return hex.EncodeToString(h[:])
}

// Stored is what the Secret holds.
type Stored struct {
	Token     string
	Username  string
	Email     string
	ClaimHash string
}

// account is the preferences s forces into Preferences.xml: its token,
// username and email, each only when it has one.
func (s Stored) account() map[string]string {
	out := map[string]string{}
	for pref, v := range map[string]string{tokenPref: s.Token, usernamePref: s.Username, mailPref: s.Email} {
		if v != "" {
			out[pref] = v
		}
	}
	return out
}

// accountIn is the account Preferences.xml at path holds.
func accountIn(path string) (Stored, error) {
	var a Stored
	for pref, v := range map[string]*string{tokenPref: &a.Token, usernamePref: &a.Username, mailPref: &a.Email} {
		got, err := plexprefs.Value(path, pref)
		if err != nil {
			return Stored{}, err
		}
		*v = got
	}
	return a, nil
}

// Store is where the token is kept outside the file.
type Store interface {
	// Get returns the zero Stored when there is nothing stored yet.
	Get(ctx context.Context) (Stored, error)
	Put(ctx context.Context, s Stored) error
}

// Exchange trades a claim code for the token of the server clientID names.
type Exchange func(ctx context.Context, claim, clientID string) (string, error)

// Starter settles the token in Preferences.xml before Plex starts.
type Starter struct {
	// Prefs is Preferences.xml.
	Prefs string
	// Claim is a plex.tv claim code; empty claims nothing.
	Claim string
	// Store is the Secret; nil keeps no copy and no record of spent codes.
	Store    Store
	Exchange Exchange
	Logger   *slog.Logger
}

// Run claims the server when Claim is a code not spent yet -- for a server
// with no token, or with a Store, any code other than the one it recorded --
// and otherwise forces the Store's account into the file when the Store
// holds a token. It never fails Plex's start: a refused claim or an
// unreadable Secret is logged and Plex starts with what the file has.
func (s Starter) Run(ctx context.Context) error {
	_, err := plexprefs.Update(s.Prefs, func(current map[string]string) (map[string]string, error) {
		return s.decide(ctx, current), nil
	})
	return err
}

func (s Starter) decide(ctx context.Context, current map[string]string) map[string]string {
	token := current[tokenPref]
	var stored Stored
	haveStore := s.Store != nil
	if haveStore {
		var err error
		if stored, err = s.Store.Get(ctx); err != nil {
			s.Logger.Warn("read the server token Secret; neither claiming over a token nor forcing its account", "error", err)
			haveStore = false
		}
	}
	if s.Claim != "" {
		hash := Hash(s.Claim)
		if token == "" || (haveStore && stored.ClaimHash != hash) {
			if fresh, ok := s.claim(ctx, current[clientIDPref]); ok {
				if haveStore {
					// The code may sign in another account, so the old
					// one's username and email go: the new ones fill in
					// from the file once Plex has written them.
					if err := s.Store.Put(ctx, Stored{Token: fresh, ClaimHash: hash}); err != nil {
						s.Logger.Warn("record the claim in the server token Secret", "error", err)
					}
				}
				out := map[string]string{tokenPref: fresh}
				for _, pref := range []string{usernamePref, mailPref} {
					if current[pref] != "" {
						out[pref] = ""
					}
				}
				return out
			}
		}
	}
	if haveStore && stored.Token != "" {
		want := stored.account()
		for pref, v := range want {
			if current[pref] != v {
				s.Logger.Info("forced the server's account from its Secret", "preference", pref)
			}
		}
		return want
	}
	return nil
}

func (s Starter) claim(ctx context.Context, clientID string) (string, bool) {
	if clientID == "" {
		s.Logger.Warn("not claiming: Preferences.xml has no ProcessedMachineIdentifier until Plex has started once; the claim is exchanged at the next start")
		return "", false
	}
	ctx, cancel := context.WithTimeout(ctx, exchangeTimeout)
	defer cancel()
	token, err := s.Exchange(ctx, s.Claim, clientID)
	if err != nil {
		s.Logger.Warn("claim the server: exchange failed; Plex starts with the token it has", "error", err)
		return "", false
	}
	s.Logger.Info("claimed the server with plex.tv")
	return token, true
}

// Keeper holds the Store and Preferences.xml to one account; the lease
// holder runs Sync every minute.
type Keeper struct {
	Prefs  string
	Store  Store
	Logger *slog.Logger
}

// Sync writes the file's account into a Store that holds no token -- the
// first lease holder does, once -- and otherwise fills a field the Store
// lacks from the file and puts the Store's account back into a file that
// drifted from it. A file with no token leaves an empty Store alone.
func (k Keeper) Sync(ctx context.Context) error {
	file, err := accountIn(k.Prefs)
	if err != nil {
		return err
	}
	stored, err := k.Store.Get(ctx)
	if err != nil {
		return err
	}
	if stored.Token == "" {
		if file.Token == "" {
			return nil
		}
		file.ClaimHash = stored.ClaimHash
		k.Logger.Info("wrote the server's account into its Secret")
		return k.Store.Put(ctx, file)
	}
	filled := stored
	if filled.Username == "" {
		filled.Username = file.Username
	}
	if filled.Email == "" {
		filled.Email = file.Email
	}
	if filled != stored {
		if err := k.Store.Put(ctx, filled); err != nil {
			return err
		}
	}
	changed, err := plexprefs.Update(k.Prefs, func(map[string]string) (map[string]string, error) {
		return filled.account(), nil
	})
	if err != nil {
		return err
	}
	if len(changed) > 0 {
		k.Logger.Warn("Preferences.xml held another account; put the Secret's back (a running Plex keeps the one it read until it restarts)",
			"preferences", changed)
	}
	return nil
}

// Secret is a Store in a Kubernetes Secret, the token under TokenKey.
type Secret struct {
	Client    kubernetes.Interface
	Namespace string
	Name      string
}

func (s Secret) Get(ctx context.Context) (Stored, error) {
	sec, err := s.Client.CoreV1().Secrets(s.Namespace).Get(ctx, s.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return Stored{}, nil
	}
	if err != nil {
		return Stored{}, fmt.Errorf("get secret %s/%s: %w", s.Namespace, s.Name, err)
	}
	return Stored{
		Token:     string(sec.Data[TokenKey]),
		Username:  string(sec.Data[UsernameKey]),
		Email:     string(sec.Data[EmailKey]),
		ClaimHash: sec.Annotations[ClaimAnnotation],
	}, nil
}

// Put creates the Secret or updates its account and claim record, keeping
// every other key and annotation; an empty field removes its key. An update carries the resourceVersion it
// read, so a concurrent writer is a conflict, retried by the next sync.
func (s Secret) Put(ctx context.Context, st Stored) error {
	secrets := s.Client.CoreV1().Secrets(s.Namespace)
	sec, err := secrets.Get(ctx, s.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = secrets.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:        s.Name,
				Namespace:   s.Namespace,
				Labels:      map[string]string{"app.kubernetes.io/managed-by": "cluster-plex"},
				Annotations: claimAnnotations(nil, st.ClaimHash),
			},
			Type: corev1.SecretTypeOpaque,
			Data: accountData(nil, st),
		}, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("create secret %s/%s: %w", s.Namespace, s.Name, err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("get secret %s/%s: %w", s.Namespace, s.Name, err)
	}
	sec.Data = accountData(sec.Data, st)
	sec.Annotations = claimAnnotations(sec.Annotations, st.ClaimHash)
	if _, err := secrets.Update(ctx, sec, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("update secret %s/%s: %w", s.Namespace, s.Name, err)
	}
	return nil
}

// accountData sets st's fields in data under their keys, removing the key
// of an empty one.
func accountData(data map[string][]byte, st Stored) map[string][]byte {
	if data == nil {
		data = map[string][]byte{}
	}
	for key, v := range map[string]string{TokenKey: st.Token, UsernameKey: st.Username, EmailKey: st.Email} {
		if v == "" {
			delete(data, key)
			continue
		}
		data[key] = []byte(v)
	}
	return data
}

func claimAnnotations(a map[string]string, hash string) map[string]string {
	if hash == "" {
		delete(a, ClaimAnnotation)
		return a
	}
	if a == nil {
		a = map[string]string{}
	}
	a[ClaimAnnotation] = hash
	return a
}
