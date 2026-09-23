package main

import (
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/go-viper/mapstructure/v2"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"

	"github.com/mediactl/clusterplex/pkg/plexdb"
	"github.com/mediactl/clusterplex/pkg/plexprefs"
)

const (
	// envPrefix turns a configuration key into an environment variable:
	// probe-port becomes CLUSTERPLEX_PROBE_PORT.
	envPrefix = "CLUSTERPLEX"
	// prefEnvPrefix introduces a single Plex preference. Everything after it
	// is the preference name, taken verbatim: Plex names are case sensitive,
	// so CLUSTERPLEX_PLEX_PREFERENCE_FriendlyName sets FriendlyName.
	prefEnvPrefix = "CLUSTERPLEX_PLEX_PREFERENCE_"
	// prefFlag repeats to set several preferences: --plex-preference Name=Value.
	prefFlag = "plex-preference"
	// machineIdentifierPref is the Plex preference machineIDKey writes.
	machineIdentifierPref = "MachineIdentifier"
	// prefKey is the configuration-file section holding preferences. It is a
	// list rather than a map because viper lowercases nested map keys, which
	// would turn FriendlyName into friendlyname and silently lose the setting.
	prefKey = "plex.preferences"
	// machineIDKey pins the Plex server identity. It is a first-class setting
	// rather than just another preference because it decides what clients and
	// plex.tv see as the server, and because it is worth validating.
	machineIDKey = "plex.machine-identifier"
	// defaultConfigFile is read when --config is not given. It is optional.
	defaultConfigFile = "/etc/clusterplex/config.yaml"
)

// Config is the manager's runtime configuration. Every field can be set by a
// command-line flag, an environment variable or a YAML file, in that order of
// precedence.
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
	// Postgres is the library database, shared by every pod. It replaces the
	// per-pod replicated SQLite file, so there is no database primary to elect
	// and no local database state to keep.
	Postgres plexdb.Config
	// BlockPlexTV keeps every pod away from Plex's own services, the lease
	// holder included.
	//
	// For a cluster that is not meant to be published, or one being kept off
	// plex.tv while something is investigated. The packets are dropped, which
	// reads to Plex as an ordinary outage and it serves regardless, but
	// nothing can claim the server or serve remotely while this is set.
	BlockPlexTV bool
	// ExternalURL is the address clients reach the proxy on. Plex advertises
	// it as a custom connection; without it Plex offers only the link-local
	// address inside its own namespace, which no client can reach.
	ExternalURL string
	// TranscodeDir is where Plex writes transcode chunks. It has to be
	// per-pod storage: every pod runs Plex and they share one volume for the
	// identity and the metadata, but a session's chunks are written and read
	// back by the single pod serving it, which the proxy pins.
	TranscodeDir string
	// ShimLibrary is the interposer preloaded into Plex so its database calls
	// reach PostgreSQL. Empty leaves Plex on its own SQLite file.
	ShimLibrary string
	// SubreaperBinary wraps Plex so its re-exec is not mistaken for an exit.
	// Empty starts Plex directly. See Supervisor.Subreaper.
	SubreaperBinary string
	// InitScript prepares the databases before Plex starts. It is upstream's
	// own, run rather than reimplemented; see cmd/manager/bootstrap.go.
	InitScript string
	// Socket is the unix socket the shim dials.
	Socket string
	// LeaseName is the Kubernetes Lease used for leader election.
	LeaseName string
	// WorkersService is the headless Service that gives pods stable DNS names.
	WorkersService string

	// PMSPort is where Plex listens inside its own network namespace, and
	// where the manager's proxy listens in the pod namespace. Plex offers no
	// way to change it.
	PMSPort int
	// PlexSubnet is the point-to-point link joining the pod namespace to
	// Plex's. Nothing outside the pod sees it, but it still must not collide
	// with a route the pod already has, so it stays configurable.
	PlexSubnet netip.Prefix
	// WorkerPort is the gRPC port on which a worker accepts jobs.
	WorkerPort int
	// ProbePort serves the health probes and metrics.
	ProbePort int

	// Preferences are the Plex settings the manager writes into
	// Preferences.xml before each start. Keys not listed here are left as
	// Plex last wrote them.
	Preferences map[string]string
}

// newFlagSet declares every flag and the default for each setting.
func newFlagSet() *pflag.FlagSet {
	fs := pflag.NewFlagSet("manager", pflag.ContinueOnError)
	fs.String("config", "", "path to a YAML configuration file (default "+defaultConfigFile+" when present)")
	fs.String("pod-name", "", "name of this pod (usually from the downward API as POD_NAME)")
	fs.String("pod-namespace", "", "namespace of this pod (usually from the downward API as POD_NAMESPACE)")
	fs.String("pms-binary", "/usr/lib/plexmediaserver/Plex Media Server", "Plex Media Server executable")
	fs.String("bin-dir", "/usr/lib/plexmediaserver", "directory holding the real Plex helper binaries")
	fs.String("plex-dir", "/var/lib/plexmediaserver/Library/Application Support/Plex Media Server", "Plex state directory")
	fs.String("socket", "/var/run/clusterplex.sock", "unix socket the shim dials")
	fs.String("lease-name", "cluster-plex-plextv", "Kubernetes Lease electing the pod that talks to plex.tv")
	fs.String("workers-service", "plex-workers", "headless Service giving pods stable DNS names")
	fs.Int("pms-port", 32400, "port Plex Media Server listens on")
	fs.String("plex-subnet", "169.254.1.0/30", "point-to-point subnet joining the pod to Plex's network namespace")
	fs.Int("worker-port", 50051, "gRPC port on which a worker accepts jobs")
	fs.Int("probe-port", 8080, "port serving health probes and metrics")
	fs.String("postgres-host", "", "host of the shared Plex library database")
	fs.Int("postgres-port", 5432, "port of the shared Plex library database")
	fs.String("postgres-database", "plex", "name of the shared Plex library database")
	fs.String("postgres-user", "plex", "user for the shared Plex library database")
	fs.String("postgres-password", "", "password for the shared Plex library database")
	fs.String("postgres-schema", "plex", "schema holding Plex's tables; required, because the shim always interpolates it into search_path")
	fs.Int("postgres-pool-size", 50, "connections the shim keeps open to the library database")
	fs.Int("postgres-pool-max", 100, "most connections the shim will open; the database max_connections must cover this times the pod count")
	fs.String("postgres-sslmode", "disable", "libpq sslmode for the library database")
	fs.Bool("block-plex-tv", false, "keep every pod away from plex.tv, the lease holder included; Plex still serves but cannot be claimed or reached remotely")
	fs.String("plex-transcode-dir", "/transcode", "per-pod directory Plex writes transcode chunks to; it must be the path the pod mounts, not shared storage")
	fs.String("plex-external-url", "", "address clients reach the proxy on, advertised to Plex clients, for example https://plex.example.com:443")
	fs.String("shim-library", ShimLibrary, "interposer preloaded into Plex so its database calls reach PostgreSQL; empty leaves Plex on its own SQLite file")
	fs.String("plex-subreaper", Subreaper, "wrapper that adopts Plex's re-exec so it is not mistaken for an exit; empty starts Plex directly")
	fs.String("init-script", InitScript, "upstream's initialisation script, run before Plex starts")
	fs.String("plex-machine-identifier", "", "UUID pinning the Plex server identity, so it survives a rebuild (default: whatever Plex generated)")
	fs.StringArray(prefFlag, nil, "Plex preference to enforce, as Name=Value (repeatable)")
	return fs
}

// loadConfig resolves the configuration from flags, the environment and an
// optional YAML file.
func loadConfig(args []string) (Config, error) {
	fs := newFlagSet()
	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}

	v := viper.New()
	v.SetEnvPrefix(envPrefix)
	v.SetEnvKeyReplacer(strings.NewReplacer("-", "_", ".", "_"))
	v.AutomaticEnv()
	if err := v.BindPFlags(fs); err != nil {
		return Config{}, fmt.Errorf("bind flags: %w", err)
	}
	// Read as plex.machine-identifier so the file groups it under plex: with
	// the preferences, while the flag stays --plex-machine-identifier.
	_ = v.BindPFlag(machineIDKey, fs.Lookup("plex-machine-identifier"))
	// The downward API sets these without the prefix.
	_ = v.BindEnv("pod-name", "POD_NAME")
	_ = v.BindEnv("pod-namespace", "POD_NAMESPACE")

	if err := readConfigFile(v, fs); err != nil {
		return Config{}, err
	}

	var errs []error
	port := func(key string) int {
		raw := strings.TrimSpace(v.GetString(key))
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 65535 {
			errs = append(errs, fmt.Errorf("%s: %q is not a valid port (1-65535)", key, raw))
			return 0
		}
		return n
	}

	c := Config{
		PodName:         v.GetString("pod-name"),
		Namespace:       v.GetString("pod-namespace"),
		PMSBinary:       v.GetString("pms-binary"),
		BinDir:          v.GetString("bin-dir"),
		PlexDir:         v.GetString("plex-dir"),
		Socket:          v.GetString("socket"),
		LeaseName:       v.GetString("lease-name"),
		WorkersService:  v.GetString("workers-service"),
		PMSPort:         port("pms-port"),
		WorkerPort:      port("worker-port"),
		ProbePort:       port("probe-port"),
		BlockPlexTV:     v.GetBool("block-plex-tv"),
		ExternalURL:     strings.TrimSpace(v.GetString("plex-external-url")),
		TranscodeDir:    strings.TrimSpace(v.GetString("plex-transcode-dir")),
		ShimLibrary:     v.GetString("shim-library"),
		SubreaperBinary: v.GetString("plex-subreaper"),
		InitScript:      v.GetString("init-script"),
		Postgres: plexdb.Config{
			Host:     v.GetString("postgres-host"),
			Port:     port("postgres-port"),
			Database: v.GetString("postgres-database"),
			User:     v.GetString("postgres-user"),
			Password: v.GetString("postgres-password"),
			Schema:   v.GetString("postgres-schema"),
			PoolSize: v.GetInt("postgres-pool-size"),
			PoolMax:  v.GetInt("postgres-pool-max"),
			SSLMode:  v.GetString("postgres-sslmode"),
		},
	}
	if err := validateExternalURL(c.ExternalURL); err != nil {
		errs = append(errs, err)
	}
	if err := c.Postgres.Validate(); err != nil {
		errs = append(errs, err)
	}

	if subnet, perr := netip.ParsePrefix(v.GetString("plex-subnet")); perr != nil {
		errs = append(errs, fmt.Errorf("plex-subnet: %w", perr))
	} else {
		c.PlexSubnet = subnet
	}

	prefs, err := loadPreferences(v, fs, os.Environ())
	if err != nil {
		errs = append(errs, err)
	}
	c.Preferences = prefs

	if c.PodName == "" || c.Namespace == "" {
		errs = append(errs, errors.New("pod-name and pod-namespace must be set (use the downward API as POD_NAME and POD_NAMESPACE)"))
	}
	return c, errors.Join(errs...)
}

// readConfigFile loads --config, or the default path when it exists.
func readConfigFile(v *viper.Viper, fs *pflag.FlagSet) error {
	path, _ := fs.GetString("config")
	explicit := path != ""
	if !explicit {
		path = defaultConfigFile
		if _, err := os.Stat(path); err != nil {
			return nil
		}
	}
	v.SetConfigFile(path)
	if err := v.ReadInConfig(); err != nil {
		return fmt.Errorf("read config file %s: %w", path, err)
	}
	return nil
}

// prefEntry is one entry of the configuration file's preference list.
type prefEntry struct {
	Name  string `mapstructure:"name"`
	Value string `mapstructure:"value"`
}

// loadPreferences merges the three sources, lowest precedence first, and
// checks the result is safe to write to Preferences.xml.
func loadPreferences(v *viper.Viper, fs *pflag.FlagSet, environ []string) (map[string]string, error) {
	prefs := map[string]string{}
	var errs []error

	var entries []prefEntry
	// WeaklyTypedInput so an unquoted YAML value such as `value: 1` is read
	// as the string "1" rather than rejected.
	err := v.UnmarshalKey(prefKey, &entries, viper.DecodeHook(mapstructure.TextUnmarshallerHookFunc()),
		func(dc *mapstructure.DecoderConfig) { dc.WeaklyTypedInput = true })
	if err != nil {
		errs = append(errs, fmt.Errorf("read %s: %w", prefKey, err))
	}
	for _, e := range entries {
		if e.Name == "" {
			errs = append(errs, fmt.Errorf("%s: an entry is missing its name", prefKey))
			continue
		}
		prefs[e.Name] = e.Value
	}

	for _, kv := range environ {
		name, value, ok := strings.Cut(kv, "=")
		if !ok || !strings.HasPrefix(name, prefEnvPrefix) {
			continue
		}
		prefs[strings.TrimPrefix(name, prefEnvPrefix)] = value
	}

	flagged, _ := fs.GetStringArray(prefFlag)
	for _, kv := range flagged {
		name, value, ok := strings.Cut(kv, "=")
		if !ok || name == "" {
			errs = append(errs, fmt.Errorf("--%s %q: expected Name=Value", prefFlag, kv))
			continue
		}
		prefs[name] = value
	}
	// Applied after the three preference sources so it can detect a conflict
	// with a MachineIdentifier declared through any of them.
	if id := strings.TrimSpace(v.GetString(machineIDKey)); id != "" {
		if declared, ok := prefs[machineIdentifierPref]; ok && declared != id {
			errs = append(errs, fmt.Errorf("%s is %q but the %s preference is %q: set one or the other",
				machineIDKey, id, machineIdentifierPref, declared))
		}
		prefs[machineIdentifierPref] = id
	}

	if len(prefs) == 0 {
		prefs = nil
	}
	if err := plexprefs.Validate(prefs); err != nil {
		errs = append(errs, err)
	}
	return prefs, errors.Join(errs...)
}

// validateExternalURL rejects an address Plex would advertise to clients but
// clients could not use. Plex hands the string over unexamined, so a bad one
// surfaces only as clients failing to connect.
func validateExternalURL(raw string) error {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("plex-external-url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("plex-external-url: %q needs an http:// or https:// scheme", raw)
	}
	if u.Host == "" {
		return fmt.Errorf("plex-external-url: %q has no host", raw)
	}
	return nil
}

// EnforcedPreferences is everything written into Preferences.xml on every
// start: what the operator declared, with the settings this architecture fixes
// applied over the top. The order matters — the forced ones win — though
// declaring one is refused at load, so it should never come to that.
func (c Config) EnforcedPreferences() map[string]string {
	prefs := make(map[string]string, len(c.Preferences))
	maps.Copy(prefs, c.Preferences)
	maps.Copy(prefs, plexprefs.Enforced(c.ExternalURL, c.TranscodeDir))
	return prefs
}

// PodDNS is this pod's stable name through the headless Service.
func (c Config) PodDNS() string {
	return fmt.Sprintf("%s.%s.%s.svc.cluster.local", c.PodName, c.WorkersService, c.Namespace)
}

// PMSAddr is how other pods reach this node's Plex Media Server.
func (c Config) PMSAddr() string { return fmt.Sprintf("%s:%d", c.PodDNS(), c.PMSPort) }

// PIDFile is where Plex records its pid.
func (c Config) PIDFile() string { return filepath.Join(c.PlexDir, "plexmediaserver.pid") }

// PreferencesFile is Plex's settings file.
func (c Config) PreferencesFile() string { return filepath.Join(c.PlexDir, "Preferences.xml") }

// DatabasesDir is where Plex keeps database files it still writes locally,
// such as its blob cache. The library itself is in PostgreSQL.
func (c Config) DatabasesDir() string {
	return filepath.Join(c.PlexDir, "Plug-in Support", "Databases")
}
