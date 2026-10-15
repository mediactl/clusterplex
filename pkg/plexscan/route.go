package plexscan

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/mediactl/clusterplex/pkg/proxy"
	"github.com/mediactl/clusterplex/pkg/remoteexec"
	pb "github.com/mediactl/clusterplex/proto"
)

// scanTimeout bounds one scan request end to end: a whole section's
// reconcile is seconds, and a client that asked for one is waiting.
const scanTimeout = 10 * time.Minute

// Diverter is the proxy's Divert for library scans (spec 2026-10-15 §5, as
// amended by the capture doc): a scan request is sent to route -- the
// manager's /api/v1/scan, on this pod or wherever the owner pointed it --
// as POST route?section=&path=&force=, and the route's answer is the
// client's. A 404 from the route means the section is not provisioned,
// and the request goes on to Plex as it was. Any other answer, a failure
// included, is relayed: Plex never scans a library the projection owns.
// A nil client is one with scanTimeout.
func Diverter(route string, client *http.Client) proxy.Divert {
	if client == nil {
		client = &http.Client{Timeout: scanTimeout}
	}
	return func(ctx context.Context, req *http.Request) (*http.Response, bool) {
		scan, ok := ParseScan(req)
		if !ok {
			return nil, false
		}
		q := url.Values{"section": {scan.Section}}
		if scan.Path != "" {
			q.Set("path", scan.Path)
		}
		if scan.Force {
			q.Set("force", "1")
		}
		target := route + "?" + q.Encode()
		post, err := http.NewRequestWithContext(ctx, http.MethodPost, target, nil)
		if err != nil {
			return textResponse(http.StatusInternalServerError, "clustarr scanner: "+err.Error()), true
		}
		resp, err := client.Do(post)
		if err != nil {
			return textResponse(http.StatusBadGateway, "clustarr scanner: "+err.Error()), true
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if resp.StatusCode == http.StatusNotFound {
			return nil, false
		}
		out := &http.Response{StatusCode: resp.StatusCode, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(body))}
		if ct := resp.Header.Get("Content-Type"); ct != "" {
			out.Header.Set("Content-Type", ct)
		}
		return out, true
	}
}

func textResponse(status int, text string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"text/plain"}}, Body: io.NopCloser(bytes.NewReader([]byte(text + "\n")))}
}

// AnalyzeRoute is remoteexec's ScanRoute: Plex still runs its binary to
// analyse an item it has just scanned, and for an item of a provisioned
// section that is answered with exit 0 without running, since the
// projection wrote the item's streams from clustarr's probe and the
// binary would only re-read the file over NFS to write the same.
type AnalyzeRoute struct {
	// Provisioned is the set of section keys the projection owns.
	Provisioned func(ctx context.Context) map[string]bool
	// SectionOf is a metadata item's section key, from the library.
	SectionOf func(ctx context.Context, item int64) (key string, ok bool)
	Logger    *slog.Logger
}

// Execute takes an analyze of a projected item; anything else is Plex's.
func (r *AnalyzeRoute) Execute(ctx context.Context, exec *pb.ExecRequest, sink remoteexec.Sink) (bool, error) {
	item, ok := ParseAnalyze(exec.GetArgs())
	if !ok {
		return false, nil
	}
	key, ok := r.SectionOf(ctx, item)
	if !ok || !r.Provisioned(ctx)[key] {
		return false, nil
	}
	_ = sink.Send(&pb.TranscodeLog{StdoutChunk: fmt.Appendf(nil, "clustarr scanner: item %d is projected from clustarr's probe; analysis skipped\n", item)})
	if r.Logger != nil {
		r.Logger.Info("analysis skipped for a projected item", "item", item, "section", key)
	}
	return true, sink.Send(&pb.TranscodeLog{ExitCode: 0, IsFinished: true})
}
