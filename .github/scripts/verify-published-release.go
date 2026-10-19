package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/ed25519"
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func must(err error) {
	if err != nil {
		panic(err)
	}
}

func main() {
	dir, tag, revision := os.Args[1], os.Args[2], os.Args[3]
	sums, err := os.ReadFile(filepath.Join(dir, "SHA256SUMS"))
	must(err)
	sigText, err := os.ReadFile(filepath.Join(dir, "SHA256SUMS.sig"))
	must(err)
	sig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sigText)))
	must(err)
	pub, err := base64.StdEncoding.DecodeString("Wywclz0tRzIUmWGXzLJv+In+uBPs423hMTvL0HPqHog=")
	must(err)
	if !ed25519.Verify(pub, append([]byte("backpack release "+tag+"\n"), sums...), sig) {
		panic("release signature does not match the pinned public key")
	}
	fmt.Println("PASS: checksum list signature matches the pinned Ed25519 key and", tag)
	listed := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(sums)), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			panic("malformed checksum line")
		}
		listed[fields[1]] = fields[0]
	}
	if len(listed) != 7 {
		panic(fmt.Sprintf("expected seven architectures, got %d", len(listed)))
	}
	for _, arch := range []string{"amd64", "arm64", "386", "s390x", "armv5", "armv6", "armv7"} {
		name := "backpack_linux_" + arch + ".tar.gz"
		data, err := os.ReadFile(filepath.Join(dir, name))
		must(err)
		hash := sha256.Sum256(data)
		if hex.EncodeToString(hash[:]) != listed[name] {
			panic("checksum mismatch: " + name)
		}
		gz, err := gzip.NewReader(bytes.NewReader(data))
		must(err)
		tr := tar.NewReader(gz)
		header, err := tr.Next()
		must(err)
		if header.Name != "backpack" || header.Typeflag != tar.TypeReg {
			panic("unexpected archive entry: " + header.Name)
		}
		binary, err := os.CreateTemp(dir, "verify-binary-*")
		must(err)
		_, copyErr := io.Copy(binary, tr)
		closeErr := binary.Close()
		must(copyErr)
		must(closeErr)
		if _, err = tr.Next(); err != io.EOF {
			panic("archive has unexpected extra entries")
		}
		must(gz.Close())
		info, err := buildinfo.ReadFile(binary.Name())
		must(err)
		settings := map[string]string{}
		for _, setting := range info.Settings {
			settings[setting.Key] = setting.Value
		}
		expectedArch := arch
		if strings.HasPrefix(arch, "armv") {
			expectedArch = "arm"
			if !strings.HasPrefix(settings["GOARM"], strings.TrimPrefix(arch, "armv")) {
				panic("wrong GOARM: " + name)
			}
		}
		if settings["GOOS"] != "linux" || settings["GOARCH"] != expectedArch || settings["CGO_ENABLED"] != "0" || settings["vcs.revision"] != revision || settings["vcs.modified"] != "false" {
			panic(fmt.Sprintf("unexpected build provenance in %s: %+v", name, settings))
		}
		must(os.Remove(binary.Name()))
		fmt.Println("PASS:", name, "checksum, archive contents, architecture, clean source revision", revision[:12])
	}
}
