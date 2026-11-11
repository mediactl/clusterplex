package logging_test

import (
	"bytes"
	"log/slog"
	"testing"

	"github.com/spf13/pflag"
	"github.com/stretchr/testify/require"

	"github.com/mediactl/clusterplex/pkg/logging"
)

func TestNewJSONFormatIsTheDefault(t *testing.T) {
	var buf bytes.Buffer
	logging.New(logging.Options{Output: &buf}).Info("hello", "k", "v")
	require.Contains(t, buf.String(), `"msg":"hello"`)
	require.Contains(t, buf.String(), `"k":"v"`)
}

func TestNewTextFormat(t *testing.T) {
	var buf bytes.Buffer
	logging.New(logging.Options{Format: "text", Output: &buf}).Info("hello", "k", "v")
	require.Contains(t, buf.String(), "msg=hello")
	require.Contains(t, buf.String(), "k=v")
	require.NotContains(t, buf.String(), "{")
}

func TestNewHonoursLevel(t *testing.T) {
	var buf bytes.Buffer
	l := logging.New(logging.Options{Format: "json", Output: &buf, Level: slog.LevelWarn})
	l.Info("dropped")
	l.Warn("kept")
	require.NotContains(t, buf.String(), "dropped")
	require.Contains(t, buf.String(), "kept")
}

func TestNewAddSourceIncludesSourceField(t *testing.T) {
	var buf bytes.Buffer
	logging.New(logging.Options{Format: "json", Output: &buf, AddSource: true}).Info("hello")
	require.Contains(t, buf.String(), `"source":`)
}

func TestNewDefaultsOutputToStderrWithoutPanicking(t *testing.T) {
	require.NotPanics(t, func() {
		logging.New(logging.Options{}).Info("goes to stderr")
	})
}

func TestBindFlagsParsesLevelFormatAndAddSource(t *testing.T) {
	opts := logging.Options{Level: slog.LevelInfo, Format: "json"}
	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
	logging.BindFlags(fs, &opts)

	require.NoError(t, fs.Parse([]string{"--log-level=debug", "--log-format=text", "--log-add-source"}))
	require.Equal(t, slog.LevelDebug, opts.Level)
	require.Equal(t, "text", opts.Format)
	require.True(t, opts.AddSource)
}

func TestBindFlagsDefaultsComeFromOptions(t *testing.T) {
	opts := logging.Options{Level: slog.LevelError, Format: "text", AddSource: true}
	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
	logging.BindFlags(fs, &opts)

	require.NoError(t, fs.Parse(nil))
	require.Equal(t, slog.LevelError, opts.Level)
	require.Equal(t, "text", opts.Format)
	require.True(t, opts.AddSource)
}

func TestLogrBridgeWritesThroughSlog(t *testing.T) {
	var buf bytes.Buffer
	lr := logging.LogrBridge(logging.New(logging.Options{Format: "json", Output: &buf}))
	lr.Info("from controller-runtime", "controller", "movie")
	require.Contains(t, buf.String(), `"msg":"from controller-runtime"`)
	require.Contains(t, buf.String(), `"controller":"movie"`)
}
