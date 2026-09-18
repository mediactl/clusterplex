package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
)

// Config is the manager's runtime configuration, read from the environment.
type Config struct {
	PodName   string
	Namespace string

	// PMSBinary is the Plex Media Server executable.
	PMSBinary string
	// BinDir holds the real helper binaries the shim stands in for.
	BinDir string
	// PlexDir is Plex's own state directory ("Plex Media Server" under the
	// application support dir).
	PlexDir string
	// LiteFSDir is where LiteFS keeps its transaction files.
	LiteFSDir string
	// Socket is the unix socket the shim dials.
	Socket string
	// LeaseName is the Kubernetes Lease used for leader election.
	LeaseName string
	// WorkersService is the headless Service that gives pods stable DNS names.
	WorkersService string

	// PMSPort is where Plex listens; Plex offers no way to change it.
	PMSPort int
	// ProxyPort is where the manager's TCP proxy listens in front of Plex.
	// Plex itself binds 32400 and 32401 and exits if either is taken, so the
	// default sits well away from them.
	ProxyPort int
	// WorkerPort is the gRPC port on which a worker accepts jobs.
	WorkerPort int
	// LiteFSPort is the replication port between LiteFS nodes.
	LiteFSPort int
	// ProbePort serves the health probes and metrics.
	ProbePort int
}

func loadConfig(getenv func(string) string) (Config, error) {
	str := func(key, def string) string {
		if v := getenv(key); v != "" {
			return v
		}
		return def
	}
	var errs []error
	port := func(key string, def int) int {
		v := getenv(key)
		if v == "" {
			return def
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 65535 {
			errs = append(errs, fmt.Errorf("%s: %q is not a valid port", key, v))
			return def
		}
		return n
	}

	c := Config{
		PodName:        getenv("POD_NAME"),
		Namespace:      getenv("POD_NAMESPACE"),
		PMSBinary:      str("CLUSTERPLEX_PMS_BINARY", "/usr/lib/plexmediaserver/Plex Media Server"),
		BinDir:         str("CLUSTERPLEX_BIN_DIR", "/usr/lib/plexmediaserver"),
		PlexDir:        str("CLUSTERPLEX_PLEX_DIR", "/var/lib/plexmediaserver/Library/Application Support/Plex Media Server"),
		LiteFSDir:      str("CLUSTERPLEX_LITEFS_DIR", "/var/lib/litefs"),
		Socket:         str("CLUSTERPLEX_SOCKET", "/var/run/clusterplex.sock"),
		LeaseName:      str("CLUSTERPLEX_LEASE_NAME", "cluster-plex-litefs"),
		WorkersService: str("CLUSTERPLEX_WORKERS_SERVICE", "plex-workers"),
		PMSPort:        port("CLUSTERPLEX_PMS_PORT", 32400),
		ProxyPort:      port("CLUSTERPLEX_PROXY_PORT", 32499),
		WorkerPort:     port("CLUSTERPLEX_WORKER_PORT", 50051),
		LiteFSPort:     port("CLUSTERPLEX_LITEFS_PORT", 20202),
		ProbePort:      port("CLUSTERPLEX_PROBE_PORT", 8080),
	}
	if c.PodName == "" || c.Namespace == "" {
		errs = append(errs, errors.New("POD_NAME and POD_NAMESPACE must be set (use the downward API)"))
	}
	return c, errors.Join(errs...)
}

// PodDNS is this pod's stable name through the headless Service.
func (c Config) PodDNS() string {
	return fmt.Sprintf("%s.%s.%s.svc.cluster.local", c.PodName, c.WorkersService, c.Namespace)
}

// PMSAddr is how other pods reach this node's Plex Media Server.
func (c Config) PMSAddr() string { return fmt.Sprintf("%s:%d", c.PodDNS(), c.PMSPort) }

// AdvertiseURL is the LiteFS replication endpoint other nodes connect to.
func (c Config) AdvertiseURL() string { return fmt.Sprintf("http://%s:%d", c.PodDNS(), c.LiteFSPort) }

// PIDFile is where Plex records its pid.
func (c Config) PIDFile() string { return filepath.Join(c.PlexDir, "plexmediaserver.pid") }

// DatabasesDir is the directory LiteFS mounts over: Plex's SQLite databases.
func (c Config) DatabasesDir() string {
	return filepath.Join(c.PlexDir, "Plug-in Support", "Databases")
}
