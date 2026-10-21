package livetv

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ProxySet serves every block's port on the link-local address, sending each
// request below the block's device endpoint on its provider's livetv Service
// (clustarr 2026-10-20-iptv-xtream-ranges-design.md). Every Plex pod runs
// one, so whichever PMS registered a block, every PMS reaches its device at
// the same http://<LocalAddress>:<Port>. The proxy names the port PMS dialled
// in X-Forwarded-Host and its root in X-Forwarded-Prefix, from which the
// tuner builds every URL it hands out, so PMS comes back the same way.
type ProxySet struct {
	LocalAddress string
	Logger       *slog.Logger

	mu      sync.Mutex
	running map[int32]*blockProxy
	closed  bool
}

type blockProxy struct {
	target string // http://<address><endpoint>
	srv    *http.Server
	done   chan struct{}
}

// target is where a block's port sends its requests: below its endpoint on
// the provider's address; "" for a block clustarr has not given one.
func target(p Provider, b Block) string {
	if p.Address == "" || b.Endpoint == "" {
		return ""
	}
	return "http://" + p.Address + b.Endpoint
}

// Apply makes the listeners exactly the blocks' ports. A listener whose
// port and target are unchanged is left alone, so open streams survive; a
// port that cannot be bound is reported and tried again on the next Apply.
func (s *ProxySet) Apply(providers []Provider) error {
	want := map[int32]string{}
	providers = slices.Clone(providers)
	slices.SortFunc(providers, func(a, b Provider) int { return strings.Compare(a.key(), b.key()) })
	for _, p := range providers {
		for _, b := range p.Blocks {
			to := target(p, b)
			if _, dup := want[b.Port]; b.Port > 0 && to != "" && !dup {
				want[b.Port] = to
			}
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("livetv: the proxy set is closed")
	}
	if s.running == nil {
		s.running = map[int32]*blockProxy{}
	}
	for port, bp := range s.running {
		if want[port] != bp.target {
			bp.stop()
			delete(s.running, port)
			s.log().Info("livetv: stopped a block's proxy", "port", port, "target", bp.target)
		}
	}
	var errs []error
	for _, port := range sortedPorts(want) {
		if _, ok := s.running[port]; ok {
			continue
		}
		listen := net.JoinHostPort(s.localAddress(), strconv.Itoa(int(port)))
		ln, err := net.Listen("tcp", listen)
		if err != nil {
			errs = append(errs, fmt.Errorf("listen %s: %w", listen, err))
			continue
		}
		bp := &blockProxy{target: want[port], done: make(chan struct{})}
		bp.srv = &http.Server{
			Handler:           blockHandler(want[port], s.log().With("component", "livetv-proxy", "port", port)),
			ReadHeaderTimeout: 10 * time.Second,
			IdleTimeout:       90 * time.Second,
		}
		go func() {
			defer close(bp.done)
			if err := bp.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				s.log().Warn("livetv: a block's proxy stopped", "listen", listen, "error", err)
			}
		}()
		s.running[port] = bp
		s.log().Info("livetv: a block's proxy is listening", "listen", listen, "target", want[port])
	}
	return errors.Join(errs...)
}

// blockHandler sends every request below to, unbuffered: a stream is
// written as it arrives.
func blockHandler(to string, log *slog.Logger) http.Handler {
	base, prefix, _ := strings.Cut(strings.TrimPrefix(to, "http://"), "/")
	prefix = "/" + prefix
	return &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.Out.URL.Scheme, r.Out.URL.Host = "http", base
			r.Out.URL.Path = prefix + r.In.URL.Path
			r.Out.URL.RawPath = ""
			r.Out.Host = base
			r.SetXForwarded()
			r.Out.Header.Set("X-Forwarded-Prefix", "/")
		},
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if r.Context().Err() == nil {
				log.Warn("livetv: the tuner did not answer a block's request", "path", r.URL.Path, "error", err)
			}
			w.WriteHeader(http.StatusBadGateway)
		},
	}
}

// Ports lists the ports being served, sorted.
func (s *ProxySet) Ports() []int32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]int32, 0, len(s.running))
	for port := range s.running {
		out = append(out, port)
	}
	slices.Sort(out)
	return out
}

// Close stops every listener and its connections.
func (s *ProxySet) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for port, bp := range s.running {
		bp.stop()
		delete(s.running, port)
	}
	s.closed = true
}

func (bp *blockProxy) stop() {
	_ = bp.srv.Close()
	<-bp.done
}

func (s *ProxySet) localAddress() string {
	if s.LocalAddress != "" {
		return s.LocalAddress
	}
	return DefaultLocalAddress
}

func (s *ProxySet) log() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}

func sortedPorts(m map[int32]string) []int32 {
	out := make([]int32, 0, len(m))
	for port := range m {
		out = append(out, port)
	}
	slices.Sort(out)
	return out
}
