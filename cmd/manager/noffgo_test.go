package main

import (
	"os/exec"
	"strings"
	"testing"
)

// The manager, shim, proxy and maintenance binaries are static and run in
// a scratch image; ffgo (purego) would make them dynamic. Only
// cmd/remux-worker may link it.
func TestNoStaticBinaryLinksFFgo(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "../manager", "../shim", "../proxy", "../maintenance").CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, out)
	}
	for _, dep := range strings.Fields(string(out)) {
		if strings.Contains(dep, "ffgo") || strings.Contains(dep, "purego") {
			t.Fatalf("a static binary links %s", dep)
		}
	}
}
