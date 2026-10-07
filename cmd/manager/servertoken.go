package main

import (
	"context"
	"time"

	"github.com/mediactl/clusterplex/pkg/plex/claim"
	"github.com/mediactl/clusterplex/pkg/plex/servertoken"
)

// defaultTokenSyncEvery is how often the lease holder holds the Secret and
// Preferences.xml to one account (servertoken.Keeper).
const defaultTokenSyncEvery = time.Minute

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
// applied: it exchanges plex.claim for the server's token, or forces the
// Secret's account -- token, username, email -- into Preferences.xml once
// the Secret holds a token (servertoken.Starter), so every replica starts
// signed in as the same account.
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

// keepServerToken holds clustarr's Secret and Preferences.xml to one
// account until ctx ends with the lease: the first lease holder writes the
// file's account into a Secret that has none, and after that the Secret's
// is put back into a file that drifts (servertoken.Keeper).
func (m *Manager) keepServerToken(ctx context.Context, store servertoken.Store) {
	log := m.Logger.With("component", "server-token")
	keeper := servertoken.Keeper{Prefs: m.Config.PreferencesFile(), Store: store, Logger: log}
	every := m.tokenSyncEvery
	if every <= 0 {
		every = defaultTokenSyncEvery
	}
	for {
		if err := keeper.Sync(ctx); err != nil && ctx.Err() == nil {
			log.Warn("hold the server's account to its Secret", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
	}
}
