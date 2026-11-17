package worker

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/mediactl/clusterplex/pkg/video"
)

// A type nobody handed a logger refuses to work rather than invent one
// (the 2026-11-10 structured-logging design §5): its lines would carry no
// service and no subsystem.
func TestANilLoggerIsRefused(t *testing.T) {
	assert.ErrorIs(t, (&Server{}).Convert(nil), errNoLogger)
}

func TestAConversionPlexEndedIsNoWarning(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logs, nil))

	logEnd(log, video.ReasonCancelled, context.Canceled)
	assert.Contains(t, logs.String(), "conversion ended by Plex")
	assert.NotContains(t, logs.String(), `"level":"WARN"`)

	logs.Reset()
	logEnd(log, video.ReasonWorkerError, errors.New("boom"))
	assert.Contains(t, logs.String(), `"level":"WARN"`)
	assert.Contains(t, logs.String(), "conversion failed")
	assert.Contains(t, logs.String(), "boom")
}
