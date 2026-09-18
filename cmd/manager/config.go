package main

import (
	"errors"
	"fmt"
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
	// proxy-port becomes CLUSTERPLEX_PROXY_PORT.
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
	// LiteFSDir is where LiteFS keeps its transaction files.
	LiteFSDir string
	// Socket is the unix socket the shim dials.
	Socket string
	// LeaseName is the Kubernetes Lease used for leader election.
	LeaseName string
	// WorkersService is the headless Service that gives pods stable DNS names.
	WorkersService string
	// SQLiteBinary is Plex's bundled SQLite, used to read the library.
	SQLiteBinary string

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

	// AdoptClusterID lets a node whose LiteFS lineage disagrees with the
	// cluster's throw its own away and resnapshot. Off by default: the node
	// that disagrees may be the one holding the only good copy.
	AdoptClusterID bool

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
	fs.String("litefs-dir", "/var/lib/litefs", "directory for LiteFS transaction files")
	fs.String("socket", "/var/run/clusterplex.sock", "unix socket the shim dials")
	fs.String("lease-name", "cluster-plex-litefs", "Kubernetes Lease used for leader election")
	fs.String("workers-service", "plex-workers", "headless Service giving pods stable DNS names")
	fs.Int("pms-port", 32400, "port Plex Media Server listens on")
	fs.Int("proxy-port", 32499, "port the manager's TCP proxy listens on")
	fs.Int("worker-port", 50051, "gRPC port on which a worker accepts jobs")
	fs.Int("litefs-port", 20202, "LiteFS replication port")
	fs.Int("probe-port", 8080, "port serving health probes and metrics")
	fs.String("sqlite-binary", plexdb.DefaultSQLite, "Plex's bundled SQLite binary, used to read the library database")
	fs.Bool("litefs-adopt-cluster-id", false, "discard this node's LiteFS lineage and resnapshot from the primary (destructive; only when the cluster's lineage is known to be the right one)")
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
		PodName:        v.GetString("pod-name"),
		Namespace:      v.GetString("pod-namespace"),
		PMSBinary:      v.GetString("pms-binary"),
		BinDir:         v.GetString("bin-dir"),
		PlexDir:        v.GetString("plex-dir"),
		LiteFSDir:      v.GetString("litefs-dir"),
		Socket:         v.GetString("socket"),
		LeaseName:      v.GetString("lease-name"),
		WorkersService: v.GetString("workers-service"),
		SQLiteBinary:   v.GetString("sqlite-binary"),
		PMSPort:        port("pms-port"),
		ProxyPort:      port("proxy-port"),
		WorkerPort:     port("worker-port"),
		LiteFSPort:     port("litefs-port"),
		ProbePort:      port("probe-port"),
		AdoptClusterID: v.GetBool("litefs-adopt-cluster-id"),
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

// PodDNS is this pod's stable name through the headless Service.
func (c Config) PodDNS() string {
	return fmt.Sprintf("%s.%s.%s.svc.cluster.local", c.PodName, c.WorkersService, c.Namespace)
}

// PMSAddr is how other pods reach this node's Plex Media Server.
func (c Config) PMSAddr() string { return fmt.Sprintf("%s:%d", c.PodDNS(), c.PMSPort) }

// AdvertiseURL is the LiteFS replication endpoint other nodes connect to.
func (c Config) AdvertiseURL() string { return fmt.Sprintf("http://%s:%d", c.PodDNS(), c.LiteFSPort) }

// LibraryDB is Plex's main library database, inside the LiteFS mount.
func (c Config) LibraryDB() string {
	return filepath.Join(c.DatabasesDir(), "com.plexapp.plugins.library.db")
}

// PIDFile is where Plex records its pid.
func (c Config) PIDFile() string { return filepath.Join(c.PlexDir, "plexmediaserver.pid") }

// PreferencesFile is Plex's settings file.
func (c Config) PreferencesFile() string { return filepath.Join(c.PlexDir, "Preferences.xml") }

// DatabasesDir is the directory LiteFS mounts over: Plex's SQLite databases.
func (c Config) DatabasesDir() string {
	return filepath.Join(c.PlexDir, "Plug-in Support", "Databases")
}
