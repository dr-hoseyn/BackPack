package app

import (
	"bytes"
	"os"
	"regexp"
	"testing"
)

// A Go security bump must also reach the source installer. Otherwise released
// binaries work, but a machine needing the source fallback refuses the new
// toolchain because its pinned checksums still describe the preceding patch.
func TestSourceInstallerCanVerifyTheRequiredGoToolchain(t *testing.T) {
	module, err := os.ReadFile("../../go.mod")
	if err != nil {
		t.Fatal(err)
	}
	installer, err := os.ReadFile("../../install.sh")
	if err != nil {
		t.Fatal(err)
	}
	installer = bytes.ReplaceAll(installer, []byte("\r\n"), []byte("\n"))
	version := regexp.MustCompile(`(?m)^go ([0-9]+\.[0-9]+\.[0-9]+)\s*$`).FindSubmatch(module)
	if len(version) != 2 {
		t.Fatal("go.mod must pin the required Go patch")
	}
	for _, key := range []string{"GO_VERSION", "GO_SHA_VERSION"} {
		found := regexp.MustCompile(`(?m)^` + key + `="([^"]+)"$`).FindSubmatch(installer)
		if len(found) != 2 || string(found[1]) != string(version[1]) {
			t.Errorf("%s does not match required Go %s", key, version[1])
		}
	}
	for _, arch := range []string{"amd64", "arm64", "386", "s390x", "armv6l"} {
		if !regexp.MustCompile(`(?m)^GO_SHA256_` + arch + `="[0-9a-f]{64}"$`).Match(installer) {
			t.Errorf("source fallback has no verified toolchain for %s", arch)
		}
	}
}
