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

func TestACloudGuideAsksForItsLineupByItsID(t *testing.T) {
	for in, want := range map[string]string{
		"lineup://tv.plex.providers.epg.cloud/657c8183afd0f02cd353633f#DIRECTV%20Mobile%20%281009%20channels%29": "657c8183afd0f02cd353633f",
		"lineup://tv.plex.providers.epg.cloud/657c8183afd0f02cd353633f":                                          "657c8183afd0f02cd353633f",
		"657c8183afd0f02cd353633f": "657c8183afd0f02cd353633f",
	} {
		assert.Equal(t, want, cloudLineupID(in), in)
	}
	c := &cloudGuides{wants: map[string]cloudWant{}, built: map[string]cloudBuilt{}, kick: make(chan struct{}, 1)}
	c.Want("dvr", "lineup://tv.plex.providers.epg.cloud/657c8183afd0f02cd353633f#DIRECTV", "http://tuner/hdhr/1", "h")
	assert.Equal(t, "657c8183afd0f02cd353633f", c.wants["dvr"].lineup, "the block's lineup URI is the service's lineup id")
}
