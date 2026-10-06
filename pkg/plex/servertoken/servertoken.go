// Package servertoken keeps the server's plex.tv token, PlexOnlineToken: it
// claims the server from a claim code before Plex starts, and mirrors the
// token into a Secret that clustarr reads (its Plex watchlist ImportList), so
// one sign-in serves both.
//
// Preferences.xml is the master copy. Every pod mounts the one file, and
// Plex itself writes the token there on a sign-in through Plex Web, so the
// Secret follows the file and never overwrites a token the file holds. The
// Secret is written back into the file only when the file has none -- a lost
// plex-config -- and it records which claim code was spent, which is what
// lets a new code re-claim a server whose old token was revoked.
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
	// ClaimAnnotation records the SHA-256 of the last claim code spent.
	ClaimAnnotation = "clusterplex.mediactl.io/claim-sha256"

	tokenPref    = "PlexOnlineToken"
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
	ClaimHash string
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
// and otherwise restores the Store's token into a file that has none. It
// never fails Plex's start: a refused claim or an unreadable Secret is
// logged and Plex starts with what the file has.
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
			s.Logger.Warn("read the server token Secret; neither claiming over a token nor restoring one", "error", err)
			haveStore = false
		}
	}

	if s.Claim != "" {
		hash := Hash(s.Claim)
		if token == "" || (haveStore && stored.ClaimHash != hash) {
			if fresh, ok := s.claim(ctx, current[clientIDPref]); ok {
				if haveStore {
					if err := s.Store.Put(ctx, Stored{Token: fresh, ClaimHash: hash}); err != nil {
						s.Logger.Warn("record the claim in the server token Secret", "error", err)
					}
				}
				return map[string]string{tokenPref: fresh}
			}
		}
	}

	if token == "" && haveStore && stored.Token != "" {
		s.Logger.Info("restored the server token from its Secret")
		return map[string]string{tokenPref: stored.Token}
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

// Mirror copies the file's token into the Store.
type Mirror struct {
	Prefs string
	Store Store
}

// Sync writes the file's token into the Store when they differ. A file with
// no token leaves the Store alone: it is the copy a restore comes from.
func (m Mirror) Sync(ctx context.Context) error {
	token, err := plexprefs.Value(m.Prefs, tokenPref)
	if err != nil || token == "" {
		return err
	}
	stored, err := m.Store.Get(ctx)
	if err != nil {
		return err
	}
	if stored.Token == token {
		return nil
	}
	stored.Token = token
	return m.Store.Put(ctx, stored)
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
	return Stored{Token: string(sec.Data[TokenKey]), ClaimHash: sec.Annotations[ClaimAnnotation]}, nil
}

// Put creates the Secret or updates its token and claim record, keeping
// every other key and annotation. An update carries the resourceVersion it
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
			Data: map[string][]byte{TokenKey: []byte(st.Token)},
		}, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("create secret %s/%s: %w", s.Namespace, s.Name, err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("get secret %s/%s: %w", s.Namespace, s.Name, err)
	}
	if sec.Data == nil {
		sec.Data = map[string][]byte{}
	}
	sec.Data[TokenKey] = []byte(st.Token)
	sec.Annotations = claimAnnotations(sec.Annotations, st.ClaimHash)
	if _, err := secrets.Update(ctx, sec, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("update secret %s/%s: %w", s.Namespace, s.Name, err)
	}
	return nil
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
