package video

import (
	"bufio"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// tokenValue finds every Plex token a fixture names, by env key, header or
// query parameter; its value must be a redaction: REDACTED, or a run of x's
// (pkg/remux's older fixtures).
var (
	tokenValue = regexp.MustCompile(`(?i)(?:X_PLEX_TOKEN|X-Plex-Token)["']?\s*[=:]\s*["']?([^"'\s&,}]*)`)
	redacted   = regexp.MustCompile(`^(REDACTED|x+)$`)
)

// secrets are what a captured Plex job must never carry at all.
var secrets = []struct {
	name string
	re   *regexp.Regexp
}{
	{"a PLEX_PG_ setting", regexp.MustCompile(`PLEX_PG_`)},
	{"an API key", regexp.MustCompile(`(?i)api_?key=[^&\s"']+`)},
	{"a password", regexp.MustCompile(`(?i)"password"`)},
}

func TestFixturesCarryNoSecrets(t *testing.T) {
	for _, root := range []string{"testdata", filepath.Join("..", "remux", "testdata")} {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			defer func() { _ = f.Close() }()
			s := bufio.NewScanner(f)
			s.Buffer(make([]byte, 1<<20), 16<<20)
			for n := 1; s.Scan(); n++ {
				for _, m := range tokenValue.FindAllStringSubmatch(s.Text(), -1) {
					if !redacted.MatchString(m[1]) {
						t.Errorf("%s:%d carries a Plex token", path, n)
					}
				}
				for _, p := range secrets {
					if p.re.MatchString(s.Text()) {
						t.Errorf("%s:%d carries %s", path, n, p.name)
					}
				}
			}
			return s.Err()
		})
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
}
