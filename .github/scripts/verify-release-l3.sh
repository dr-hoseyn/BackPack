#!/usr/bin/env bash
# Run as an ordinary Actions user; only fresh network/mount namespaces use root.
set -euo pipefail
: "${BIN:?current release binary required}"
: "${PREV_BIN:?previous stable binary required}"
: "${RUNNER_TEMP:?disposable workspace required}"

cases=(
  udp:normal:1 udp:normal:2 quic:normal:1 pck:normal:1
  sni:normal:1 xdi:normal:1 spoof:normal:1
  udp:restart:1 pck:restart:1 pck:flow:1
  udp:loss:1 quic:loss:1
  udp:old-listener:1 udp:old-dialler:1
  quic:old-listener:1 quic:old-dialler:1
  pck:old-listener:1 pck:old-dialler:1
)
for entry in "${cases[@]}"; do
  IFS=: read -r carrier scenario paths <<< "$entry"
  dial="$BIN"; listen="$BIN"; extra=()
  case "$scenario" in
    restart) extra+=(RESTART=1) ;;
    flow) extra+=(BLOCKFLOW=1) ;;
    loss) extra+=("NETEM=delay 20ms loss 1%") ;;
    old-listener) listen="$PREV_BIN" ;;
    old-dialler) dial="$PREV_BIN" ;;
  esac
  work="$RUNNER_TEMP/release-l3-$carrier-$scenario-$paths"
  sudo env BIN="$dial" BIN_LISTEN="$listen" W="$work" "${extra[@]}" \
    timeout -k 5s 6m unshare --net --mount --fork -- bash internal/e2e/testdata/l3live.sh "$carrier" "$paths" \
    | tee "$work.out"
  grep -q '^RESULT OK' "$work.out"
done
