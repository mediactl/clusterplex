package main

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/mediactl/clusterplex/pkg/logging"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The process's first record carries its service (the 2026-11-10
// structured-logging design §3): service.name proxy under cluster-plex.
func TestItsFirstLineCarriesItsService(t *testing.T) {
	var out bytes.Buffer
	_, log, err := newRoot(t.Context(), logging.Options{Format: "json"}, &out)
	require.NoError(t, err)
	log.Info("started")
	line, _, _ := bytes.Cut(out.Bytes(), []byte("\n"))
	var rec struct {
		Service struct{ Name, Namespace string } `json:"service"`
	}
	require.NoError(t, json.Unmarshal(line, &rec), "%s", out.String())
	assert.Equal(t, "proxy", rec.Service.Name)
	assert.Equal(t, "cluster-plex", rec.Service.Namespace)
}
