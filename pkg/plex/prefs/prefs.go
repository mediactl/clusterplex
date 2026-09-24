// Package prefs maintains Plex Media Server's Preferences.xml.
//
// The file is a single self-closing <Preferences/> element whose attributes
// are the settings. Plex owns it at runtime and writes things the manager
// must never invent, above all the server identity and the plex.tv token, so
// this package merges rather than generates: declared keys are set, every
// other attribute is carried across untouched.
package prefs

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
)

const (
	rootElement = "Preferences"
	xmlHeader   = `<?xml version="1.0" encoding="utf-8"?>`
	// fileMode is owner-only because Preferences.xml holds the plex.tv token.
	fileMode = 0o600
)

// machineIdentifier is the server's identity. Pinning it is how every pod
// presents the same server across a failover and how a rebuilt cluster keeps
// the identity clients already know, so it is settable.
const machineIdentifier = "MachineIdentifier"

// derivedFrom maps a preference Plex computes to the one it computes it from.
// Plex derives these once and never recomputes them, so changing a source
// while leaving the old derived value in place would leave the two
// disagreeing. Removing the derived value makes Plex regenerate it.
//
// ProcessedMachineIdentifier is what clients and plex.tv actually see as the
// server ID. Plex derives it from MachineIdentifier deterministically but with
// a salt we cannot reproduce, so it can only be regenerated, never computed
// here.
var derivedFrom = map[string]string{"ProcessedMachineIdentifier": machineIdentifier}

// uuidPattern is the form Plex writes for MachineIdentifier. Rejecting
// anything else stops a typo from reaching Plex, where it would silently
// become a different server.
var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// attrName is the subset of XML attribute names Plex uses: a letter or
// underscore, then letters, digits, underscores, dots or dashes. It admits
// keys like "_10de1f141a58200c00000100.0-TranscodeCountLimit" and excludes
// anything that would need a namespace or produce invalid XML.
var attrName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.\-]*$`)

// attr is one preference, in file order.
type attr struct {
	Name  string
	Value string
}

// Validate reports whether an operator may declare every key. It is called at
// configuration load so a bad key fails startup rather than the first leader
// election. It is stricter than what Apply will write: settings this
// architecture fixes are written by the manager but refused here, because an
// operator declaring one is a misunderstanding worth failing on rather than a
// value to silently overwrite.
func Validate(values map[string]string) error {
	errs := []error{writable(values)}
	for _, name := range slices.Sorted(maps.Keys(values)) {
		if forced := forcedBy(name); forced != "" {
			errs = append(errs, fmt.Errorf("preference %q cannot be declared: %s", name, forced))
		}
	}
	return errors.Join(errs...)
}

// writable reports whether every key can go into the file at all, whoever set
// it. Apply uses this rather than Validate because the manager writes the
// forced settings that Validate exists to refuse.
func writable(values map[string]string) error {
	var errs []error
	for _, name := range slices.Sorted(maps.Keys(values)) {
		switch source := derivedFrom[name]; {
		case !attrName.MatchString(name):
			errs = append(errs, fmt.Errorf("preference %q is not a valid Plex preference name", name))
		case source != "":
			errs = append(errs, fmt.Errorf("preference %q is derived by Plex from %s and cannot be set directly; set %s instead", name, source, source))
		case name == machineIdentifier && !uuidPattern.MatchString(values[name]):
			errs = append(errs, fmt.Errorf("preference %s must be a UUID such as 9c67996e-8b08-44b9-9c83-a6d317322a2d, got %q", name, values[name]))
		}
	}
	return errors.Join(errs...)
}

// Apply merges values into the Preferences.xml at path and returns the keys
// whose value it changed, sorted. It writes nothing when the file already
// agrees, so a restart that changes no setting leaves the file alone. A file
// that cannot be parsed is reported and left untouched.
func Apply(path string, values map[string]string) ([]string, error) {
	if err := writable(values); err != nil {
		return nil, err
	}

	// Held across the read and the write together, not around the write
	// alone: what is lost without it is one pod's merge landing on top of a
	// copy another pod read before it. See lockPreferences.
	unlock, err := lockPreferences(path)
	if err != nil {
		return nil, err
	}
	defer unlock()

	current, err := read(path)
	if err != nil {
		return nil, err
	}

	merged, changed := merge(current, values)
	if len(changed) == 0 {
		return nil, nil
	}
	if err := writeAtomic(path, render(merged)); err != nil {
		return nil, err
	}
	return changed, nil
}

// read returns the attributes of an existing file, or nil when it does not
// exist yet or is empty.
// Value returns one setting from Preferences.xml, or "" when the file or the
// setting is absent. Reading is what the manager does with the server's own
// token, which lives here and nowhere else.
func Value(path, name string) (string, error) {
	attrs, err := read(path)
	if err != nil {
		return "", err
	}
	for _, a := range attrs {
		if a.Name == name {
			return a.Value, nil
		}
	}
	return "", nil
}

func read(path string) ([]attr, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return nil, nil
	}
	return parse(b)
}

// parse extracts the attributes of the <Preferences> element.
func parse(b []byte) ([]attr, error) {
	dec := xml.NewDecoder(bytes.NewReader(b))
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("no <%s> element found", rootElement)
		}
		if err != nil {
			return nil, fmt.Errorf("parse preferences: %w", err)
		}
		se, ok := tok.(xml.StartElement)
		if !ok || se.Name.Local != rootElement {
			continue
		}
		attrs := make([]attr, 0, len(se.Attr))
		for _, a := range se.Attr {
			name := a.Name.Local
			if a.Name.Space != "" {
				name = a.Name.Space + ":" + a.Name.Local
			}
			attrs = append(attrs, attr{Name: name, Value: a.Value})
		}
		return attrs, nil
	}
}

// merge overlays values onto current, keeping the file's existing order and
// appending keys it did not already carry in sorted order.
func merge(current []attr, values map[string]string) ([]attr, []string) {
	out := slices.Clone(current)
	var changed []string

	seen := make(map[string]bool, len(out))
	for i, a := range out {
		want, declared := values[a.Name]
		if !declared {
			continue
		}
		seen[a.Name] = true
		if a.Value != want {
			out[i].Value = want
			changed = append(changed, a.Name)
		}
	}

	var added []string
	for name := range values {
		if !seen[name] {
			added = append(added, name)
		}
	}
	slices.Sort(added)
	for _, name := range added {
		out = append(out, attr{Name: name, Value: values[name]})
		changed = append(changed, name)
	}

	out, changed = invalidateDerived(out, changed)
	slices.Sort(changed)
	return out, changed
}

// invalidateDerived removes any value Plex computed from a preference that
// just changed, so that Plex recomputes it on its next start.
func invalidateDerived(out []attr, changed []string) ([]attr, []string) {
	for _, derived := range slices.Sorted(maps.Keys(derivedFrom)) {
		if !slices.Contains(changed, derivedFrom[derived]) {
			continue
		}
		if i := slices.IndexFunc(out, func(a attr) bool { return a.Name == derived }); i >= 0 {
			out = slices.Delete(out, i, i+1)
			changed = append(changed, derived)
		}
	}
	return out, changed
}

// render writes the element in the shape Plex itself uses.
func render(attrs []attr) []byte {
	var b bytes.Buffer
	b.WriteString(xmlHeader)
	b.WriteString("\n<")
	b.WriteString(rootElement)
	for _, a := range attrs {
		b.WriteString(" ")
		b.WriteString(a.Name)
		b.WriteString(`="`)
		var esc bytes.Buffer
		_ = xml.EscapeText(&esc, []byte(a.Value))
		b.Write(esc.Bytes())
		b.WriteString(`"`)
	}
	b.WriteString("/>\n")
	return b.Bytes()
}

// writeAtomic replaces path in one step, so a crash mid-write cannot leave
// Plex with a half-written configuration file.
func writeAtomic(path string, content []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("create temp file in %s: %w", dir, err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()

	if err := tmp.Chmod(fileMode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return nil
}
