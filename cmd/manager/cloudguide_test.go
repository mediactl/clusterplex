package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mediactl/clusterplex/pkg/plex/guidebuild"
)

func TestAPlansChannelsAreTheMapsGridKeysAndNumbers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[
			{"channelKey":"657c-k1","deviceIdentifier":"3","lineupIdentifier":"003"},
			{"channelKey":"657c-k2","deviceIdentifier":"78","lineupIdentifier":"078"},
			{"channelKey":"657c-k2","deviceIdentifier":"203","lineupIdentifier":"203"},
			{"channelKey":"657c-k2","deviceIdentifier":"203","lineupIdentifier":"203"}
		]`))
	}))
	defer srv.Close()
	got, err := (&cloudGuides{}).mapChannels(context.Background(), cloudWant{mapURI: srv.URL})
	require.NoError(t, err)
	want := []guidebuild.Channel{{GridKey: "k1", VCN: "003"}, {GridKey: "k2", VCN: "078"}, {GridKey: "k2", VCN: "203"}}
	assert.Equal(t, want, got, "a station under two numbers is two channels; one channel twice is one")

	p := cloudPlan{Channels: got}
	assert.Equal(t, []string{"k1", "k2"}, p.gridKeys(), "a grid key is fetched once")

	// A plan written before plans carried numbers is no plan: the holder
	// writes the next.
	var old cloudPlan
	assert.Error(t, json.Unmarshal([]byte(`{"dvr":"d","lineup":"l","channels":["k1"]}`), &old))
}
