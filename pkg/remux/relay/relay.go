// Package relay is the manager's side of the remux pool: it hands a
// browser remux to the worker the hash ring names, writes the segments it
// streams into Plex's session directory, and only then tells Plex about
// them. It links no ffgo.
package relay

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/mediactl/clusterplex/pkg/hashring"
	"github.com/mediactl/clusterplex/pkg/remoteexec"
	"github.com/mediactl/clusterplex/pkg/remux"
	pb "github.com/mediactl/clusterplex/proto"
	"github.com/mediactl/clusterplex/proto/remuxpb"
)

// segmentName is the only file a worker may name.
var segmentName = regexp.MustCompile(`^(init-stream[01]|chunk-stream[01]-\d{5,})\.m4s$`)

type Relay struct {
	Players remux.Players
	Workers remoteexec.WorkerLister
	Dial    func(ctx context.Context, addr string) (remuxpb.RemuxClient, io.Closer, error)
	PMS     string
	HTTP    *http.Client
	Logger  *slog.Logger
}

func (r *Relay) log() *slog.Logger {
	if r.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return r.Logger
}

func (r *Relay) client() *http.Client {
	if r.HTTP == nil {
		return http.DefaultClient
	}
	return r.HTTP
}

func (r *Relay) Execute(ctx context.Context, req *pb.ExecRequest, sink remoteexec.Sink) (bool, error) {
	job, ok := remux.Classify(ctx, req, r.Players)
	if !ok {
		return false, nil
	}
	ws, err := r.Workers.ListReady(ctx)
	if err != nil || len(ws) == 0 {
		return false, nil
	}
	names := make([]string, len(ws))
	addr := map[string]string{}
	for i, w := range ws {
		names[i], addr[w.Name] = w.Name, w.Addr
	}
	pick := hashring.New(names...).Locate(job.RingKey())
	log := r.log().With("worker", pick, "session", job.SessionID(), "segment", job.SkipToSegment)

	c, closer, err := r.Dial(ctx, addr[pick])
	if err != nil {
		log.Warn("dial remux worker failed; Plex runs the job", "error", err)
		return false, nil
	}
	defer func() { _ = closer.Close() }()
	stream, err := c.Remux(ctx, job.Proto())
	if err != nil {
		log.Warn("remux worker refused the job; Plex runs it", "error", err)
		return false, nil
	}
	first := true
	for {
		ev, err := stream.Recv()
		if err != nil {
			if first {
				log.Warn("remux worker failed before writing a file; Plex runs the job", "error", err)
				return false, nil
			}
			if errors.Is(err, io.EOF) {
				err = errors.New("remux worker ended the stream without Done")
			}
			return true, finish(sink, err)
		}
		switch k := ev.Kind.(type) {
		case *remuxpb.Event_File:
			// Until the first file the session directory is untouched, so a
			// failure can still be handed to Plex's transcoder.
			first = false
			if err := writeFile(req.GetCwd(), k.File.GetName(), k.File.GetData()); err != nil {
				return true, finish(sink, err)
			}
		case *remuxpb.Event_Manifest:
			if err := r.send(ctx, http.MethodPost, job.ManifestURL, "", "", k.Manifest.GetMpd(), job.Token); err != nil {
				return true, finish(sink, fmt.Errorf("post manifest: %w", err))
			}
		case *remuxpb.Event_Progress:
			// Plex's own transcoder carries on past a failed progress PUT too.
			_ = r.send(ctx, http.MethodPut, job.ProgressURL, k.Progress.GetPath(), k.Progress.GetQuery(), nil, job.Token)
		case *remuxpb.Event_Done:
			var err error
			if msg := k.Done.GetError(); msg != "" {
				err = errors.New(msg)
				if first {
					log.Warn("remux worker refused the job before writing a file; Plex runs the job", "error", err)
					return false, nil
				}
			}
			return true, finish(sink, err)
		}
	}
}

// finish tells the shim how the job ended, as Plex's transcoder's exit code.
func finish(sink remoteexec.Sink, err error) error {
	code := int32(0)
	if err != nil {
		code = 1
		_ = sink.Send(&pb.TranscodeLog{StderrChunk: []byte("remux: " + err.Error() + "\n")})
	}
	_ = sink.Send(&pb.TranscodeLog{ExitCode: code, IsFinished: true})
	return err
}

func writeFile(dir, name string, data []byte) error {
	if !segmentName.MatchString(name) {
		return fmt.Errorf("remux: refusing file name %q", name)
	}
	tmp := filepath.Join(dir, "."+name+".tmp")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, name))
}

// send makes a request to Plex at its own address: the job's URL names
// 127.0.0.1:32400, which from the manager's namespace is the proxy.
// sub is appended to the path ("stream", "streamDetail"); a non-empty query
// replaces the URL's own.
func (r *Relay) send(ctx context.Context, method, raw, sub, query string, body []byte, token string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	base, err := url.Parse(r.PMS)
	if err != nil {
		return err
	}
	u.Scheme, u.Host = base.Scheme, base.Host
	if sub != "" {
		u.Path += "/" + sub
	}
	if query != "" {
		u.RawQuery = query
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	if token != "" {
		req.Header.Set("X-Plex-Token", token)
	}
	resp, err := r.client().Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: %s", method, u.Path, resp.Status)
	}
	return nil
}

// maxSegmentBytes bounds one File event, a whole segment: grpc-go's 4 MiB
// default is under 10 s of high-bitrate 1080p, and any 4K remux.
const maxSegmentBytes = 256 << 20

// GRPCDialer dials a remux worker over plaintext gRPC inside the cluster.
func GRPCDialer(_ context.Context, addr string) (remuxpb.RemuxClient, io.Closer, error) {
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(maxSegmentBytes)))
	if err != nil {
		return nil, nil, err
	}
	return remuxpb.NewRemuxClient(conn), conn, nil
}
