#!/usr/bin/env bash
# Only called inside a new mount namespace on an ephemeral Linux CI runner.
set -euo pipefail
test "$(id -u)" = 0
test "$(readlink /proc/self/ns/mnt)" != "$(readlink /proc/1/ns/mnt)"
test "$(readlink /proc/self/ns/net)" != "$(readlink /proc/1/ns/net)"
mount --make-rprivate /
ip link set lo up
work=$(mktemp -d)
for pair in 'config:/etc/backpack' 'services:/etc/systemd/system' 'install:/root/BackPack' 'bin:/usr/local/bin'; do
  source_dir="$work/${pair%%:*}"
  target_dir="${pair#*:}"
  install -d -m 700 "$source_dir"
  mkdir -p "$target_dir"
  mount --bind "$source_dir" "$target_dir"
done
BP_UPDATE_HEALTH_INSTALL=1 go test -race ./internal/manage \
  -run '^TestUpdateHealthLocalUpdateRestoresBinaryAndConfiguration$' -count=1 -v -timeout 1m
