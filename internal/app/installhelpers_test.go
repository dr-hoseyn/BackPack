package app

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func installerHelperFunctions(t *testing.T) string {
	t.Helper()
	body, err := os.ReadFile("../../install.sh")
	if err != nil {
		t.Fatal(err)
	}
	source := strings.ReplaceAll(string(body), "\r\n", "\n")
	start, end := strings.Index(source, "helpers_defaults() {"), strings.Index(source, "# End optional protocol helpers.")
	if start < 0 || end < start {
		t.Fatal("installer helper functions not found")
	}
	return source[start:end]
}

func TestInstallerInstallsHTTPSHelpersByDefaultAndHonorsOverrides(t *testing.T) {
	functions := installerHelperFunctions(t)
	for _, tc := range []struct {
		name, setting, arch, libc, want string
		fail                            bool
	}{
		{"default-amd64", "unset BP_HELPERS", "amd64", "glibc", "INSTALL naive\nINSTALL sing-box\nINSTALL xray", false},
		{"default-arm64", "unset BP_HELPERS", "arm64", "glibc", "INSTALL naive\nINSTALL sing-box\nINSTALL xray", false},
		{"explicit-opt-out", "BP_HELPERS=", "amd64", "glibc", "", false},
		{"only-xray", "BP_HELPERS=xray", "amd64", "glibc", "INSTALL xray", false},
		{"only-naive", "BP_HELPERS=naive", "amd64", "glibc", "INSTALL naive\nINSTALL sing-box", false},
		{"musl-default", "unset BP_HELPERS", "amd64", "musl", "INSTALL xray", false},
		{"legacy-architecture", "unset BP_HELPERS", "386", "glibc", "", false},
		{"explicit-unsupported-naive", "BP_HELPERS=naive", "amd64", "musl", "", true},
		{"explicit-unsupported-architecture", "BP_HELPERS=xray", "386", "glibc", "", true},
		{"invalid-selection", "BP_HELPERS=invalid", "amd64", "glibc", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prefix := "set -euo pipefail\nARCH=" + tc.arch + "\n" + tc.setting + "\n" +
				"uname() { echo Linux; }\ngetconf() { [[ '" + tc.libc + "' == glibc ]]; }\n" +
				"info() { :; }; warn() { :; }; err() { echo \"$*\" >&2; }\n"
			script := prefix + functions + "\ninstall_helper() { printf 'INSTALL %s\\n' \"$1\"; }\nhelpers_defaults\nhelpers_validate\ninstall_helpers\n"
			out, err := runInstallerHelperScript(script)
			if (err != nil) != tc.fail {
				t.Fatalf("unexpected result: %v\n%s", err, out)
			}
			if tc.fail {
				if strings.Contains(string(out), "INSTALL ") {
					t.Fatalf("installed helpers after failed validation: %s", out)
				}
			} else if strings.TrimSpace(string(out)) != tc.want {
				t.Fatalf("installed %q, want %q", out, tc.want)
			}
		})
	}
}

func TestInstallerFetchesOnlyMissingHelperPrerequisites(t *testing.T) {
	for _, tc := range []struct {
		name            string
		installed, fail bool
	}{
		{"unzip-missing", false, false}, {"all-tools-present", true, false}, {"package-install-fails", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := "0"
			if tc.installed {
				state = "1"
			}
			ret := "0"
			if tc.fail {
				ret = "1"
			}
			script := "set -euo pipefail\nBP_HELPERS=naive,xray\nhave_unzip=" + state + "\n" +
				"info() { :; }; warn() { :; }; err() { echo \"$*\" >&2; }\n" +
				installerHelperFunctions(t) + `
command() {
  [[ "$1" == -v ]] || { builtin command "$@"; return; }
  case "$2" in
    unzip) [[ "$have_unzip" == 1 ]] ;;
    apt-get|curl|xz|tar|sha256sum) return 0 ;;
    *) return 1 ;;
  esac
}

apt-get() {
  printf 'APT %s\n' "$*"
  [[ "$1" != install ]] || have_unzip=1
  return ` + ret + `
}
helpers_require_tools
`
			out, err := runInstallerHelperScript(script)
			if (err != nil) != tc.fail {
				t.Fatalf("unexpected result: %v\n%s", err, out)
			}
			if tc.installed && len(out) != 0 {
				t.Fatalf("unnecessary package installation: %s", out)
			}
			if !tc.installed && !tc.fail && strings.TrimSpace(string(out)) != "APT update\nAPT install -y --no-install-recommends unzip" {
				t.Fatalf("wrong prerequisites: %s", out)
			}
		})
	}
}

func TestInstallerNaiveCompatibilityPreservesExistingHelpers(t *testing.T) {
	script := "set -euo pipefail\n" + installerHelperFunctions(t) + `
root="$(mktemp -d)"
trap 'rm -rf -- "$root"' EXIT
for ARCH in amd64 arm64; do
  helper_spec naive "$ARCH"
  mkdir "$root/$helper_version-$ARCH"
  printf 'new helper' > "$root/$helper_version-$ARCH/naive"
  helper_compat_path
  legacy="$root/v154.0.8037.49-4-$ARCH"
  [[ "$(cat "$legacy/naive")" == 'new helper' ]]
  [[ "$(uname -s)" != Linux || -L "$legacy" ]]
  helper_compat_path
  rm -rf -- "$legacy"
  mkdir "$legacy"
  printf 'existing helper' > "$legacy/naive"
  helper_compat_path
  [[ ! -L "$legacy" && "$(cat "$legacy/naive")" == 'existing helper' ]]
done
`
	if out, err := runInstallerHelperScript(script); err != nil {
		t.Fatalf("helper compatibility failed: %v\n%s", err, out)
	}
}

func TestInstallerDownloadFailureIsVisible(t *testing.T) {
	body, err := os.ReadFile("../../install.sh")
	if err != nil {
		t.Fatal(err)
	}
	source := strings.ReplaceAll(string(body), "\r\n", "\n")
	start, end := strings.Index(source, "fetch() {"), strings.Index(source, "# verify_asset <file>")
	if start < 0 || end < start {
		t.Fatal("installer fetch function not found")
	}
	script := "info() { :; }; err() { echo \"$*\" >&2; }\n" + source[start:end] + `
curl() { echo 'curl: (22) HTTP 404' >&2; return 22; }
fetch https://example.invalid/helper archive
`
	out, err := runInstallerHelperScript(script)
	if err == nil || !strings.Contains(string(out), "HTTP 404") || !strings.Contains(string(out), "Download failed: https://example.invalid/helper") {
		t.Fatalf("download failure hidden: %v\n%s", err, out)
	}
}

func runInstallerHelperScript(script string) ([]byte, error) {
	cmd := exec.Command("bash", "-s")
	cmd.Stdin = strings.NewReader(script)
	return cmd.CombinedOutput()
}
