package livetv

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/mediactl/clusterplex/pkg/proxy"
)

// ProxySet serves every block's port on the link-local address, piping each
// connection to its provider's livetv Service (Live TV spec §5.0). Every
// Plex pod runs one, so whichever PMS registered a block, every PMS reaches
// its device at the same http://<LocalAddress>:<Port>. The proxy is
// byte-level: the Host header carries the port to the tuner, which routes
// by it.
type ProxySet struct {
	LocalAddress string
	Logger       *slog.Logger

	mu      sync.Mutex
	running map[int32]*blockProxy
	closed  bool
}

type blockProxy struct {
	target string
	tcp    *proxy.TCP
	cancel context.CancelFunc
}

// Apply makes the listeners exactly the blocks' ports. A listener whose
// port and target are unchanged is left alone, so open streams survive; a
// port that cannot be bound is reported and tried again on the next Apply.
// Two providers claiming one port is clustarr's to prevent; here the first
// in name order keeps it.
func (s *ProxySet) Apply(providers []Provider) error {
	want := map[int32]string{}
	providers = slices.Clone(providers)
	slices.SortFunc(providers, func(a, b Provider) int { return strings.Compare(a.key(), b.key()) })
	for _, p := range providers {
		if p.Address == "" {
			continue
		}
		for _, b := range p.Blocks {
			if _, dup := want[b.Port]; b.Port > 0 && !dup {
				want[b.Port] = p.Address
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
		ctx, cancel := context.WithCancel(context.Background())
		tcp := &proxy.TCP{Listen: listen, Target: want[port], Logger: s.log().With("component", "livetv-proxy", "port", port)}
		if _, err := tcp.Start(ctx); err != nil {
			cancel()
			errs = append(errs, fmt.Errorf("listen %s: %w", listen, err))
			continue
		}
		s.running[port] = &blockProxy{target: want[port], tcp: tcp, cancel: cancel}
		s.log().Info("livetv: a block's proxy is listening", "listen", listen, "target", want[port])
	}
	return errors.Join(errs...)
}

// Ports is the ports being served, in order.
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
	bp.cancel()
	bp.tcp.Wait()
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
