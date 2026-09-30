package provision

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"slices"

	plexapi "github.com/mediactl/clusterplex/pkg/plex/api"
)

// ProviderRef is a provider to register, by its root.
type ProviderRef struct {
	URI string `mapstructure:"uri"`
}

// Library is a library section to create on a provider's agent.
type Library struct {
	Name      string   `mapstructure:"name"`
	Type      string   `mapstructure:"type"`
	Provider  string   `mapstructure:"provider"`
	Language  string   `mapstructure:"language"`
	Locations []string `mapstructure:"locations"`
	// SwitchAgent lets the provisioner move an existing library of this
	// name onto the provider's agent. Off, a library on another agent is
	// reported as drifted and left alone.
	SwitchAgent bool `mapstructure:"switchAgent"`
}

// Config is what the provisioner reconciles.
type Config struct {
	Providers []ProviderRef
	Libraries []Library
}

// Result is how far a run got.
type Result string

const (
	Converged Result = "converged"
	Pending   Result = "pending"
	Errored   Result = "error"
)

var scanners = map[string]string{"movie": "Plex Movie", "show": "Plex TV Series"}

// Validate checks what can be checked without asking anyone.
func (c Config) Validate() error {
	var errs []error
	for _, p := range c.Providers {
		u, err := url.Parse(p.URI)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			errs = append(errs, fmt.Errorf("metadata provider %q: not an absolute http(s) URL", p.URI))
		}
	}
	seen := map[string]bool{}
	for _, l := range c.Libraries {
		switch {
		case l.Name == "":
			errs = append(errs, errors.New("library: missing name"))
		case seen[l.Name]:
			errs = append(errs, fmt.Errorf("library %q: declared twice", l.Name))
		}
		seen[l.Name] = true
		if _, ok := scanners[l.Type]; !ok {
			errs = append(errs, fmt.Errorf("library %q: type %q is not movie or show", l.Name, l.Type))
		}
		if l.Provider == "" {
			errs = append(errs, fmt.Errorf("library %q: missing provider", l.Name))
		}
		if len(l.Locations) == 0 {
			errs = append(errs, fmt.Errorf("library %q: no locations", l.Name))
		}
		for _, loc := range l.Locations {
			if !path.IsAbs(loc) {
				errs = append(errs, fmt.Errorf("library %q: location %q is not absolute", l.Name, loc))
			}
		}
	}
	return errors.Join(errs...)
}

// ProviderFor is the provider of the first library of a section type, or "".
func (c Config) ProviderFor(sectionType string) string {
	for _, l := range c.Libraries {
		if l.Type == sectionType {
			return l.Provider
		}
	}
	return ""
}

// LibraryNames are the configured libraries' names: the only ones the
// seeder writes into (ADR 0006).
func (c Config) LibraryNames() []string {
	var out []string
	for _, l := range c.Libraries {
		out = append(out, l.Name)
	}
	return out
}

// Provisioner reconciles Config into one Plex.
type Provisioner struct {
	PMS *plexapi.Client
	// HTTP fetches provider roots; nil is a client with a 30 s timeout.
	HTTP   *http.Client
	Config Config
	Logger *slog.Logger
	// Drift is told, each run, whether each library is on another agent.
	Drift func(library string, drifted bool)
}

// Run makes one idempotent pass. Pending means a provider root did not
// answer yet, so its libraries were skipped; the caller retries sooner.
func (p *Provisioner) Run(ctx context.Context) (Result, error) {
	log := p.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	pending := false

	roots := map[string]Root{}  // identifier -> root
	uris := map[string]string{} // identifier -> uri
	for _, ref := range p.Config.Providers {
		root, err := FetchRoot(ctx, p.HTTP, ref.URI)
		if err != nil {
			log.Info("metadata provider not answering yet", "uri", ref.URI, "error", err)
			pending = true
			continue
		}
		roots[root.Identifier] = root
		uris[root.Identifier] = ref.URI
	}

	providers, err := p.PMS.Providers(ctx)
	if err != nil {
		return Errored, err
	}
	for ident, uri := range uris {
		i := slices.IndexFunc(providers, func(pr plexapi.Provider) bool { return pr.Identifier == ident })
		switch {
		case i < 0:
			if err := p.PMS.AddProvider(ctx, uri); err != nil && !errors.Is(err, plexapi.ErrConflict) {
				return Errored, err
			}
			log.Info("registered metadata provider", "identifier", ident, "uri", uri)
		case providers[i].URI != uri:
			if err := p.PMS.UpdateProvider(ctx, providers[i].ID, uri); err != nil {
				return Errored, err
			}
			log.Info("moved metadata provider", "identifier", ident, "uri", uri)
		}
	}

	groups, err := p.ensureGroups(ctx, roots)
	if err != nil {
		return Errored, err
	}

	sections, err := p.PMS.Sections(ctx)
	if err != nil {
		return Errored, err
	}
	var errs []error
	for _, lib := range p.Config.Libraries {
		if _, ok := roots[lib.Provider]; !ok {
			if pending {
				continue // its root may be the one not answering yet
			}
			errs = append(errs, fmt.Errorf("library %q: no configured provider declares %q", lib.Name, lib.Provider))
			continue
		}
		if err := p.ensureLibrary(ctx, log, lib, groups[lib.Provider], sections); err != nil {
			errs = append(errs, err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return Errored, err
	}
	if pending {
		return Pending, nil
	}
	return Converged, nil
}

// ensureGroups creates an agent for every resolved provider that has none,
// and returns each provider's group id. A group an operator combined with
// other providers is used as it is.
func (p *Provisioner) ensureGroups(ctx context.Context, roots map[string]Root) (map[string]int, error) {
	groups, err := p.PMS.Groups(ctx)
	if err != nil {
		return nil, err
	}
	created := false
	for ident, root := range roots {
		if slices.ContainsFunc(groups, func(g plexapi.Group) bool { return g.PrimaryIdentifier == ident }) {
			continue
		}
		title := root.Title
		if title == "" {
			title = ident
		}
		if err := p.PMS.AddGroup(ctx, title, ident); err != nil {
			return nil, err
		}
		created = true
	}
	if created {
		if groups, err = p.PMS.Groups(ctx); err != nil {
			return nil, err
		}
	}
	out := map[string]int{}
	for _, g := range groups {
		if _, ok := out[g.PrimaryIdentifier]; !ok {
			out[g.PrimaryIdentifier] = g.ID
		}
	}
	return out, nil
}

func (p *Provisioner) ensureLibrary(ctx context.Context, log *slog.Logger, lib Library, groupID int, sections []plexapi.Section) error {
	language := lib.Language
	if language == "" {
		language = "en-US"
	}
	i := slices.IndexFunc(sections, func(s plexapi.Section) bool { return s.Title == lib.Name })
	if i < 0 {
		err := p.PMS.CreateSection(ctx, plexapi.NewSection{
			Name: lib.Name, Type: lib.Type, Agent: lib.Provider,
			Scanner: scanners[lib.Type], Language: language, GroupID: groupID, Locations: lib.Locations,
		})
		if err != nil {
			return fmt.Errorf("library %q: create: %w", lib.Name, err)
		}
		log.Info("created library", "library", lib.Name, "agent", lib.Provider)
		p.drift(lib.Name, false)
		return nil
	}
	s := sections[i]
	if !sameLocations(s.Location, lib.Locations) {
		log.Warn("library locations differ from the configuration; left as they are",
			"library", lib.Name, "configured", lib.Locations)
	}
	if s.Agent == lib.Provider {
		p.drift(lib.Name, false)
		return nil
	}
	if !lib.SwitchAgent {
		log.Warn("library is on another agent; set switchAgent to move it",
			"library", lib.Name, "agent", s.Agent, "configured", lib.Provider)
		p.drift(lib.Name, true)
		return nil
	}
	if err := p.PMS.SetSectionAgent(ctx, s.Key, lib.Provider, groupID); err != nil {
		p.drift(lib.Name, true)
		return fmt.Errorf("library %q: switch agent: %w", lib.Name, err)
	}
	// A new agent otherwise applies only to items added from now on.
	if err := p.PMS.RefreshSection(ctx, s.Key, "", true); err != nil {
		return fmt.Errorf("library %q: refresh after switching agent: %w", lib.Name, err)
	}
	log.Info("moved library onto its configured agent", "library", lib.Name, "from", s.Agent, "to", lib.Provider)
	p.drift(lib.Name, false)
	return nil
}

func (p *Provisioner) drift(lib string, drifted bool) {
	if p.Drift != nil {
		p.Drift(lib, drifted)
	}
}

func sameLocations(have []plexapi.Location, want []string) bool {
	got := make([]string, 0, len(have))
	for _, l := range have {
		got = append(got, l.Path)
	}
	w := append([]string(nil), want...)
	slices.Sort(got)
	slices.Sort(w)
	return slices.Equal(got, w)
}
