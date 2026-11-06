package app

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func installerGoFunctions(t *testing.T) string {
	t.Helper()
	src, err := os.ReadFile("../../install.sh")
	if err != nil {
		t.Fatal(err)
	}
	sh := strings.ReplaceAll(string(src), "\r\n", "\n")
	start := strings.Index(sh, "go_new_enough() {")
	build := strings.Index(sh, "build_from_source() (")
	if start < 0 || build < start {
		t.Fatal("installer Go functions missing")
	}
	end := strings.Index(sh[build:], "\n)\n")
	if end < 0 {
		t.Fatal("installer build function missing its end")
	}
	return sh[start : build+end+3]
}

func TestInstallerChecksTheBundledGoPatchVersion(t *testing.T) {
	functions := installerGoFunctions(t)
	for _, tc := range []struct {
		version string
		want    bool
	}{
		{"1.26.5", false}, {"1.26.6", true}, {"1.26.10", true},
		{"1.25.99", false}, {"1.27.0", true}, {"1.27rc1", false},
	} {
		t.Run(tc.version, func(t *testing.T) {
			// Auto mode pretends to be current; local mode exposes the older compiler.
			fake := `fake_go() { if [[ "$GOTOOLCHAIN" == local ]]; then echo 'go version go` + tc.version + ` linux/amd64'; else echo 'go version go1.26.6 linux/amd64'; fi; }`
			cmd := exec.Command("bash", "-c", "export GOTOOLCHAIN=auto\nGO_VERSION=1.26.6\nGO_MIN_MINOR=26\n"+functions+"\n"+fake+"\ngo_new_enough fake_go")
			out, err := cmd.CombinedOutput()
			if (err == nil) != tc.want {
				t.Fatalf("accepted=%v want %v: %s (%v)", err == nil, tc.want, out, err)
			}
		})
	}
}

func TestInstallerBuildsWithTheSelectedGoBinary(t *testing.T) {
	functions := installerGoFunctions(t)
	for _, tc := range []struct {
		name, pathVersion, localVersion, want string
		failBuild                             bool
	}{
		{"new-path-old-local", "1.26.6", "1.26.5", "PATH", false},
		{"old-path-new-local", "1.26.5", "1.26.6", "LOCAL", false},
		{"both-too-old", "1.26.5", "1.26.5", "", false},
		{"build-fails-after-writing", "1.26.6", "1.26.5", "PATH", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, fake := range []struct{ name, version, label string }{
				{"path-bin", tc.pathVersion, "PATH"}, {"local-bin", tc.localVersion, "LOCAL"},
			} {
				body := "#!/usr/bin/env bash\n" + `if [[ "$1" == version ]]; then
  if [[ "$GOTOOLCHAIN" == local ]]; then echo 'go version go` + fake.version + ` linux/amd64'; else echo 'go version go1.26.6 linux/amd64'; fi
elif [[ "$1" == build ]]; then
  [[ "$GOTOOLCHAIN" == local ]] || exit 3
  while [[ "$1" != -o ]]; do shift; done
  echo 'BUILT_WITH_` + fake.label + `' > "$2"
  echo 'BUILT_WITH_` + fake.label + `'
  [[ "${FAIL_BUILD:-0}" != 1 ]]
else exit 4; fi
`
				if err := os.Mkdir(filepath.Join(dir, fake.name), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, fake.name, "go"), []byte(body), 0700); err != nil {
					t.Fatal(err)
				}
			}
			// Redirect the system paths before running any installer functions.
			block := strings.NewReplacer("/usr/local/go/bin/go", `"$PWD/local-bin/go"`,
				"install_go_toolchain /usr/local", `install_go_toolchain "$PWD"`).Replace(functions)
			prefix := "set -euo pipefail\nexport GOTOOLCHAIN=auto\nGO_VERSION=1.26.6\nGO_MIN_MINOR=26\nSCRIPT_DIR=$PWD\nINSTALL_DIR=$PWD\nBIN_PATH=$PWD/backpack\nexport PATH=\"$PWD/path-bin:$PATH\"\n"
			if tc.failBuild {
				prefix += "export FAIL_BUILD=1\n"
			}
			stubs := "\ninfo() { echo \"$*\"; }; warn() { echo \"$*\"; }; err() { echo \"$*\" >&2; }; download_go() { echo DOWNLOAD_REQUIRED >&2; return 1; };\n"
			stubs += "publish_binary() { mv -- \"$1\" \"$BIN_PATH\"; };\necho 'old binary' > \"$BIN_PATH\"\n"
			cmd := exec.Command("bash", "-c", prefix+block+stubs+"build_from_source")
			cmd.Dir = dir
			out, err := cmd.CombinedOutput()
			if tc.want == "" {
				if err == nil || !strings.Contains(string(out), "DOWNLOAD_REQUIRED") || strings.Contains(string(out), "BUILT_WITH_") {
					t.Fatalf("old compilers must request installation before build: %s (%v)", out, err)
				}
			} else if (err != nil) != tc.failBuild || !strings.Contains(string(out), "BUILT_WITH_"+tc.want) {
				t.Fatalf("expected selected %s compiler: %s (%v)", tc.want, out, err)
			}
			if tc.failBuild {
				got, err := os.ReadFile(filepath.Join(dir, "backpack"))
				if err != nil || strings.TrimSpace(string(got)) != "old binary" {
					t.Fatalf("failed build changed installed binary: %q (%v)", got, err)
				}
				matches, err := filepath.Glob(filepath.Join(dir, "backpack.*"))
				if err != nil || len(matches) != 0 {
					t.Fatalf("failed build leaked staging files: %v (%v)", matches, err)
				}
			}
		})
	}
}

func TestInstallerKeepsExistingGoWhenToolchainReplacementFails(t *testing.T) {
	script := `set -euo pipefail
stage="$(mktemp -d)"
trap 'rm -rf -- "$stage"' EXIT
prefix="$stage/local"
archive="$stage/download.tgz"
GO_VERSION=1.26.6
mkdir -p "$prefix/go/bin" "$stage/archive/go/bin"
echo 'old compiler' > "$prefix/go/bin/go"
err() { echo "$*" >&2; }
` + installerGoFunctions(t) + `
download_go() { cp "$archive" "$1"; }
printf 'incomplete download' > "$archive"
if install_go_toolchain "$prefix"; then
  exit 1
fi
[[ "$(cat "$prefix/go/bin/go")" == 'old compiler' ]]

cat > "$stage/archive/go/bin/go" <<'EOF'
#!/usr/bin/env bash
echo 'go version go1.26.6 linux/amd64'
EOF
chmod +x "$stage/archive/go/bin/go"
tar -czf "$archive" -C "$stage/archive" go
mv() {
  if [[ "$3" == "$prefix"/.backpack-go.*/go ]]; then
    return 1
  fi
  command mv "$@"
}
if install_go_toolchain "$prefix"; then
  exit 1
fi
[[ "$(cat "$prefix/go/bin/go")" == 'old compiler' ]]
unset -f mv
install_go_toolchain "$prefix"
go_new_enough "$prefix/go/bin/go"
shopt -s nullglob
staging=("$prefix"/.backpack-go.*)
[[ "${#staging[@]}" -eq 0 ]]
`
	if out, err := runInstallerHelperScript(script); err != nil {
		t.Fatalf("toolchain staging or rollback failed: %v\n%s", err, out)
	}
}
