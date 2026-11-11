package version_test

import (
	"strings"
	"testing"

	"github.com/mediactl/clusterplex/pkg/version"
)

// A binary built without the Makefile's -X flag still names a version, the
// one every record carries as service.version.
func TestVersionDefaultsToDev(t *testing.T) {
	if version.Version != "0.0.0-dev" {
		t.Errorf("Version = %q, want the 0.0.0-dev default", version.Version)
	}
}

func TestStringJoinsVersionAndCommit(t *testing.T) {
	got := version.String()
	if !strings.Contains(got, version.Version) || !strings.Contains(got, version.Commit) || !strings.Contains(got, "+") {
		t.Errorf("String() = %q, want <version>+<commit>", got)
	}
}
