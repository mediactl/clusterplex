package plexscan

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mediactl/clusterplex/pkg/plexproject"
	pb "github.com/mediactl/clusterplex/proto"
)

// scanAPI is a fake of the manager's /api/v1/scan: it records what it was
// asked and answers as told.
type scanAPI struct {
	status        int
	body          string
	unprovisioned bool
	asked         []string
	token         string
}

func (a *scanAPI) server(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.asked = append(a.asked, r.Method+" "+r.URL.RequestURI())
		a.token = r.Header.Get("X-Plex-Token")
		if a.unprovisioned {
			w.Header().Set(UnprovisionedHeader, "1")
		}
		w.WriteHeader(a.status)
		_, _ = io.WriteString(w, a.body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/api/v1/scan"
}

// A scan request goes to the configured route as a POST naming the
// section, the folder and force; the route's 200 is the client's answer,
// so Plex never scans.
func TestDivertSendsAScanToTheRouteAndRelaysItsAnswer(t *testing.T) {
	api := &scanAPI{status: http.StatusOK, body: `{"applied":2}`}
	d := Diverter(api.server(t), nil)
	req := httptest.NewRequest(http.MethodPost, "/library/sections/1/refresh?path=%2Fmedia%2Fmovies%2FHeat+%281995%29&force=1", nil)
	resp, claimed := d(context.Background(), req)
	require.True(t, claimed)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)
	require.Equal(t, `{"applied":2}`, string(body))
	require.Equal(t, []string{"POST /api/v1/scan?force=1&path=%2Fmedia%2Fmovies%2FHeat+%281995%29&section=1"}, api.asked)
}

// A route that says the section is not provisioned -- a 404 carrying the
// unprovisioned header -- declines: the request goes on to Plex as it
// was. A bare 404 (a misconfigured route) is a failure, relayed (I3).
func TestDivertDeclinesOnlyWhenTheRouteSaysUnprovisioned(t *testing.T) {
	api := &scanAPI{status: http.StatusNotFound, body: "section 9 is not provisioned", unprovisioned: true}
	d := Diverter(api.server(t), nil)
	_, claimed := d(context.Background(), httptest.NewRequest(http.MethodPost, "/library/sections/9/refresh", nil))
	require.False(t, claimed)
	require.Len(t, api.asked, 1)

	bare := &scanAPI{status: http.StatusNotFound, body: "404 page not found"}
	d = Diverter(bare.server(t), nil)
	resp, claimed := d(context.Background(), httptest.NewRequest(http.MethodPost, "/library/sections/9/refresh", nil))
	require.True(t, claimed, "a bare 404 is the route missing, not the section")
	require.Equal(t, http.StatusBadGateway, resp.StatusCode)
}

// I4: the client's Plex token travels with the scan, so the endpoint can
// refuse a request Plex itself would have refused.
func TestDivertForwardsTheClientsPlexToken(t *testing.T) {
	api := &scanAPI{status: http.StatusOK}
	d := Diverter(api.server(t), nil)
	req := httptest.NewRequest(http.MethodPost, "/library/sections/1/refresh?X-Plex-Token=abc", nil)
	_, _ = d(context.Background(), req)
	require.Equal(t, "abc", api.token)
	req = httptest.NewRequest(http.MethodPost, "/library/sections/1/refresh", nil)
	req.Header.Set("X-Plex-Token", "hdr")
	_, _ = d(context.Background(), req)
	require.Equal(t, "hdr", api.token)
}

// A route that fails is still the answer: the client sees the failure
// rather than Plex running its own scan over a library this design owns.
func TestDivertAnswersARouteFailureItself(t *testing.T) {
	api := &scanAPI{status: http.StatusServiceUnavailable, body: "no lease holder"}
	d := Diverter(api.server(t), nil)
	resp, claimed := d(context.Background(), httptest.NewRequest(http.MethodPost, "/library/sections/1/refresh", nil))
	require.True(t, claimed)
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
}

// A request that is not a library scan is never sent anywhere.
func TestDivertLeavesEveryOtherRequestAlone(t *testing.T) {
	api := &scanAPI{status: http.StatusOK}
	d := Diverter(api.server(t), nil)
	_, claimed := d(context.Background(), httptest.NewRequest(http.MethodPut, "/library/metadata/7/refresh", nil))
	require.False(t, claimed)
	require.Empty(t, api.asked)
}

type collectSink struct{ msgs []*pb.TranscodeLog }

func (c *collectSink) Send(m *pb.TranscodeLog) error { c.msgs = append(c.msgs, m); return nil }

func (c *collectSink) final() *pb.TranscodeLog {
	for _, m := range c.msgs {
		if m.GetIsFinished() {
			return m
		}
	}
	return nil
}

func (c *collectSink) stdout() string {
	var b strings.Builder
	for _, m := range c.msgs {
		b.Write(m.GetStdoutChunk())
	}
	return b.String()
}

// An analyze of an item in a provisioned section is answered with exit 0
// and never run: the projection wrote its streams.
func TestAnAnalyzeOfAProjectedItemExitsZeroWithoutRunning(t *testing.T) {
	r := &AnalyzeRoute{
		Projected: func(_ context.Context, item int64) bool { return item == 14371 },
	}
	sink := &collectSink{}
	done, err := r.Execute(context.Background(), &pb.ExecRequest{TargetBinary: "Plex Media Scanner", Args: []string{"--analyze", "--log-file-suffix", "Analysis", "--no-thumbs", "--item", "14371"}}, sink)
	require.True(t, done)
	require.NoError(t, err)
	require.NotNil(t, sink.final())
	require.Equal(t, int32(0), sink.final().GetExitCode())
	require.Contains(t, sink.stdout(), "14371")

	done, _ = r.Execute(context.Background(), &pb.ExecRequest{TargetBinary: "Plex Media Scanner", Args: []string{"--analyze", "--item", "500"}}, &collectSink{})
	require.False(t, done, "an item the projection does not own: Plex's binary runs")
	done, _ = r.Execute(context.Background(), &pb.ExecRequest{TargetBinary: "Plex Media Scanner", Args: []string{"--scan", "--section", "1"}}, &collectSink{})
	require.False(t, done, "not an analyze: left alone")
}

// Outcome's JSON is what the scan endpoint answers and the divert relays.
func TestOutcomeRoundTripsAsJSON(t *testing.T) {
	b, err := json.Marshal(plexproject.Outcome{Applied: 1, Unknown: 2})
	require.NoError(t, err)
	require.JSONEq(t, `{"Applied":1,"Deleted":0,"Adopted":0,"Unknown":2,"Failed":0}`, string(b))
}
