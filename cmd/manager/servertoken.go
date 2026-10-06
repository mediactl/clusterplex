package main

import (
	"context"
	"time"

	"github.com/mediactl/clusterplex/pkg/plex/claim"
	"github.com/mediactl/clusterplex/pkg/plex/servertoken"
)

// defaultMirrorEvery is how often the lease holder copies the server's
// token into clustarr's Secret when it differs.
const defaultMirrorEvery = time.Minute

// tokenStore is the Secret named by plex.clustarr.tokenSecret, in clustarr's
// namespace; nil when none is named.
func (m *Manager) tokenStore() servertoken.Store {
	cl := m.Config.Clustarr
	if !cl.Enabled || cl.TokenSecret == "" || m.K8sClient == nil {
		return nil
	}
	return servertoken.Secret{Client: m.K8sClient, Namespace: cl.Namespace, Name: cl.TokenSecret}
}

// settleServerToken runs before each Plex start, after the preferences are
// applied: it exchanges plex.claim for the server's token, or restores the
// token from the Secret into a Preferences.xml that lost it
// (servertoken.Starter).
func (m *Manager) settleServerToken(ctx context.Context) error {
	exchange := m.claimExchange
	if exchange == nil {
		exchange = claim.Exchanger{}.Exchange
	}
	s := servertoken.Starter{
		Prefs:    m.Config.PreferencesFile(),
		Claim:    m.Config.Claim,
		Exchange: exchange,
		Logger:   m.Logger.With("component", "server-token"),
	}
	if store := m.tokenStore(); store != nil {
		s.Store = store
	}
	return s.Run(ctx)
}

// mirrorServerToken keeps clustarr's Secret holding the file's token, until
// ctx ends with the lease.
func (m *Manager) mirrorServerToken(ctx context.Context, store servertoken.Store) {
	log := m.Logger.With("component", "server-token")
	mirror := servertoken.Mirror{Prefs: m.Config.PreferencesFile(), Store: store}
	every := m.mirrorEvery
	if every <= 0 {
		every = defaultMirrorEvery
	}
	for {
		if err := mirror.Sync(ctx); err != nil && ctx.Err() == nil {
			log.Warn("copy the server token into clustarr's Secret", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
	}
}
