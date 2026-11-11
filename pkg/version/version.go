package version

import "fmt"

// Version is the build every record carries as service.version and every
// span's resource as service.version: set at link time with
// -X github.com/mediactl/clusterplex/pkg/version.Version=<git describe>
// (the Makefile and the three Dockerfiles), else the development default.
var Version = "0.0.0-dev"

// Commit is the git revision the binary was built from, set the same way.
var Commit = "unknown"

// String renders the build identity as "<version>+<commit>".
func String() string {
	return fmt.Sprintf("%s+%s", Version, Commit)
}
