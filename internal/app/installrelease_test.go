package app

import (
	"os"
	"strings"
	"testing"
)

func TestInstallerValidatesLocalReleasePreparation(t *testing.T) {
	functions := installerReleaseFunctions(t)
	for _, tc := range []struct {
		name, setup, want string
	}{
		{"missing checksum", `echo 'unrelated entry' > "$SCRIPT_DIR/SHA256SUMS"`, "could not be verified"},
		{"failed copy", `cp() { return 1; }`, "Could not copy local release asset"},
		{"offline without checksum", "", "archive ready"},
		{"verified offline", `(cd "$SCRIPT_DIR" && sha256sum "$ASSET" > SHA256SUMS)`, "archive ready"},
		{"Windows checksum file", `(cd "$SCRIPT_DIR" && sha256sum -t "$ASSET" | awk '{printf "%s *%s\r\n", toupper($1), $2}' > SHA256SUMS)`, "archive ready"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			script := functions + `
SCRIPT_DIR="$stage/source"
INSTALL_DIR="$stage/cache"
mkdir "$SCRIPT_DIR" "$INSTALL_DIR"
printf 'new archive' > "$SCRIPT_DIR/$ASSET"
printf 'old archive' > "$INSTALL_DIR/$ASSET"
trusted_dir() { return 0; }
` + tc.setup + `
if install_release; then
  [[ "$(cat "$INSTALL_DIR/$ASSET")" == 'new archive' ]]
  echo 'archive ready'
fi
`
			out, err := runInstallerHelperScript(script)
			wantSuccess := tc.want == "archive ready"
			if (err == nil) != wantSuccess || !strings.Contains(string(out), tc.want) {
				t.Fatalf("expected %q, got %v\n%s", tc.want, err, out)
			}
		})
	}
}

func TestInstallerKeepsExecutableWhenExtractionFails(t *testing.T) {
	script := installerReleaseFunctions(t) + `
INSTALL_DIR="$stage"
BIN_PATH="$stage/installed"
printf 'old binary' > "$BIN_PATH"
printf 'broken archive' > "$stage/$ASSET"
if install_binary_from_tar; then
  echo 'installed broken archive' >&2
  exit 1
fi
[[ "$(cat "$BIN_PATH")" == 'old binary' ]]
printf 'new binary' > "$stage/backpack"
tar -czf "$stage/$ASSET" -C "$stage" backpack
install_binary_from_tar
[[ "$(cat "$BIN_PATH")" == 'new binary' ]]
[[ "$(cat "$stage/install_path")" == "$stage" ]]
shopt -s nullglob
staged_files=("$BIN_PATH".*)
[[ "${#staged_files[@]}" -eq 0 ]]
`
	if out, err := runInstallerHelperScript(script); err != nil {
		t.Fatalf("release extraction failed: %v\n%s", err, out)
	}
}

func installerReleaseFunctions(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("../../install.sh")
	if err != nil {
		t.Fatal(err)
	}
	source := strings.ReplaceAll(string(raw), "\r\n", "\n")
	start := strings.Index(source, "verify_asset() {")
	end := strings.Index(source, "# Build-from-source fallback")
	if start < 0 || end < start {
		t.Fatal("installer release functions missing")
	}
	prefix := `set -euo pipefail
stage="$(mktemp -d)"
trap 'rm -rf -- "$stage"' EXIT
ASSET=backpack_linux_amd64.tar.gz
info() { echo "$*"; }
warn() { echo "$*" >&2; }
err() { echo "$*" >&2; }
fetch() { echo "unexpected network download" >&2; exit 3; }
`
	return prefix + strings.ReplaceAll(source[start:end], "/etc/backpack/install_path", `"$stage/install_path"`)
}
