package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	plexcodecs "github.com/mediactl/clusterplex/pkg/plex/codecs"
)

func TestTheCodecsLiveTVStreamsCarryArePrefetchedByDefault(t *testing.T) {
	podIdentity(t)
	c, err := loadConfig(nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"aac", "ac3", "eac3", "mp2", "mp3"}, c.CodecPrefetch)
	assert.Equal(t, "/var/lib/plexmediaserver/Library/Application Support/Plex Media Server/Codecs", c.CodecsDir())
}

func TestTheCodecPrefetchListIsReadFromEverySource(t *testing.T) {
	for name, tc := range map[string]struct {
		args []string
		env  string
		file string
		want []string
	}{
		"flag":                {args: []string{"--plex-codec-prefetch=aac, dca"}, want: []string{"aac", "dca"}},
		"environment":         {env: "AAC,truehd", want: []string{"aac", "truehd"}},
		"file list":           {file: "plex-codec-prefetch: [aac, flac]\n", want: []string{"aac", "flac"}},
		"file string":         {file: "plex-codec-prefetch: \"eac3 mp2\"\n", want: []string{"eac3", "mp2"}},
		"none turns it off":   {env: "none", want: nil},
		"empty turns it off":  {args: []string{"--plex-codec-prefetch="}, want: nil},
		"a repeat is dropped": {env: "aac,aac", want: []string{"aac"}},
	} {
		t.Run(name, func(t *testing.T) {
			podIdentity(t)
			if tc.env != "" {
				t.Setenv("CLUSTERPLEX_PLEX_CODEC_PREFETCH", tc.env)
			}
			args := tc.args
			if tc.file != "" {
				args = append(args, "--config", writeConfig(t, tc.file))
			}
			c, err := loadConfig(args)
			require.NoError(t, err)
			assert.Equal(t, tc.want, c.CodecPrefetch)
		})
	}
}

func TestACodecPrefetchListThatNamesNoCodecIsRefused(t *testing.T) {
	for _, v := range []string{"aac;ac3", "none,aac", "../aac"} {
		t.Run(v, func(t *testing.T) {
			podIdentity(t)
			t.Setenv("CLUSTERPLEX_PLEX_CODEC_PREFETCH", v)
			_, err := loadConfig(nil)
			assert.ErrorContains(t, err, "plex-codec-prefetch")
		})
	}
}

func TestTheCodecPrefetchRetriesUntilNothingIsLeftToRetry(t *testing.T) {
	results := []struct {
		res plexcodecs.Result
		err error
	}{
		{err: errors.New("connection refused")},
		{res: plexcodecs.Result{Fetched: []string{"ac3"}, Failed: []string{"aac"}}},
		{res: plexcodecs.Result{Fetched: []string{"aac"}, NoItem: []string{"mp2"}}},
	}
	runs := 0
	prefetch := func(context.Context) (plexcodecs.Result, error) {
		r := results[runs]
		runs++
		return r.res, r.err
	}
	done := make(chan struct{})
	go func() {
		runCodecPrefetch(t.Context(), prefetch, time.Millisecond, 4*time.Millisecond, slog.New(slog.NewTextHandler(io.Discard, nil)))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("still retrying after a run that left nothing to retry")
	}
	assert.Equal(t, 3, runs, "a codec no library item carries is not retried")
}

func TestTheCodecPrefetchStopsWithTheLease(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	runs := 0
	prefetch := func(context.Context) (plexcodecs.Result, error) {
		runs++
		if runs == 2 {
			cancel()
		}
		return plexcodecs.Result{Failed: []string{"aac"}}, nil
	}
	done := make(chan struct{})
	go func() {
		runCodecPrefetch(ctx, prefetch, time.Millisecond, time.Millisecond, slog.New(slog.NewTextHandler(io.Discard, nil)))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the prefetch outlived its context")
	}
	assert.Equal(t, 2, runs)
}

func TestOnlyALeaseHolderThatReachesPlexTVPrefetches(t *testing.T) {
	// Plex downloads codecs from plex.tv: with every pod kept off it there
	// is nothing to wait for but a retry an hour.
	m := newTestManager()
	m.Config.CodecPrefetch = []string{"aac"}
	m.Config.BlockPlexTV = true
	assert.False(t, m.prefetchesCodecs())
	m.Config.BlockPlexTV = false
	assert.False(t, m.prefetchesCodecs(), "no library database to find an item in")
}
