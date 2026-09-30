package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	plexprovision "github.com/mediactl/clusterplex/pkg/plex/provision"
)

func TestTheLeaseHolderProvisionsAndStopsWhenItLetsGo(t *testing.T) {
	var adds atomic.Int32
	pms := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/media/providers/metadata":
			adds.Add(1)
		case r.URL.Path == "/plex/movies":
			_, _ = io.WriteString(w, `{"MediaProvider":{"identifier":"tv.plex.agents.custom.x","title":"X"}}`)
		default:
			_, _ = io.WriteString(w, `{"MediaContainer":{}}`)
		}
	}))
	defer pms.Close()

	m := newTestManager()
	m.plexAddr = strings.TrimPrefix(pms.URL, "http://")
	m.Config.Provision = plexprovision.Config{Providers: []plexprovision.ProviderRef{{URI: pms.URL + "/plex/movies"}}}
	m.provisionEvery = 20 * time.Millisecond

	m.takePlexTV(t.Context())
	require.Eventually(t, func() bool { return adds.Load() >= 2 }, 5*time.Second, 10*time.Millisecond,
		"the provider never appears in the fake's list, so each resync adds it again")
	m.releasePlexTV(t.Context())
	stopped := adds.Load()
	time.Sleep(100 * time.Millisecond)
	assert.LessOrEqual(t, adds.Load(), stopped+1, "no more passes once the lease is gone")
}

func TestNothingToProvisionStartsNothing(t *testing.T) {
	m := newTestManager()
	m.takePlexTV(t.Context())
	m.stopLeaderWork()
	m.stopLeaderWork() // idempotent
}
