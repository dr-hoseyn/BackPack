#!/usr/bin/env bash
set -euo pipefail
binary="$(realpath "$1")"
work="$(mktemp -d /tmp/backpack-kernel-ct-XXXXXX)"
first="bpkcta-$$"; second="bpkctb-$$"
pids=()
stop_pair() {
    for process in "${pids[@]}"; do kill -TERM "$process" 2>/dev/null || true; done
    for process in "${pids[@]}"; do wait "$process" 2>/dev/null || true; done
    pids=()
}
cleanup() {
    stop_pair
    ip netns del "$first" 2>/dev/null || true
    ip netns del "$second" 2>/dev/null || true
    if [[ -n "${SUDO_UID:-}" ]]; then chown -R "$SUDO_UID:${SUDO_GID:-$SUDO_UID}" "$work"; fi
}
trap cleanup EXIT
ip netns add "$first"; ip netns add "$second"
ip link add bpkcta-veth type veth peer name bpkctb-veth
ip link set bpkcta-veth netns "$first"; ip link set bpkctb-veth netns "$second"
for pair in "$first 1 bpkcta-veth" "$second 2 bpkctb-veth"; do
    read -r namespace host device <<< "$pair"
    ip -n "$namespace" link set lo up
    ip -n "$namespace" addr add "192.0.2.$host/24" dev "$device"
    ip -n "$namespace" -6 addr add "2001:db8:1::$host/64" dev "$device" nodad
    ip -n "$namespace" link set "$device" up
done
for mode in ${MODES:-direct reverse}; do
    for method in ${METHODS:-gre ipip 6to4 6to4-gre sit ip6gre ipip6 ip6ip6 wireguard xfrm-ipsec fou-gre gue-ipip ecmp-lanes eoip gretap ip6gretap vxlan geneve l2tpv3 etherip}; do
        path="$work/$mode-$method"; mkdir -p "$path"
        reverse=0; [[ "$mode" == reverse ]] && reverse=1
        for pair in "$first iran" "$second kharej"; do
            read -r namespace role <<< "$pair"
            ip netns exec "$namespace" env BP_KERNEL_CT_METHOD="$method" BP_KERNEL_CT_REVERSE="$reverse" BP_KERNEL_CT_DIR="$path" BP_KERNEL_CT_ROLE="$role" BP_KERNEL_CT_CANCEL="${CANCEL:-0}" unshare --mount --propagation private bash -c '
                mount -t tmpfs tmpfs /run
                charon=""
                if [[ "$BP_KERNEL_CT_METHOD" == xfrm-ipsec ]]; then
                    /usr/lib/ipsec/charon > "$BP_KERNEL_CT_DIR/charon-$BP_KERNEL_CT_ROLE.log" 2>&1 & charon=$!
                    for attempt in $(seq 1 50); do [[ -S /run/charon.vici ]] && break; sleep .1; done
                fi
                trap '\''[[ -z "$charon" ]] || kill "$charon" 2>/dev/null || true'\'' EXIT
                "$1" -test.run "^TestKernelConnectionTestLiveEndpoint$" -test.v -test.timeout 130s & runner=$!
                trap '\''kill "$runner" 2>/dev/null || true; [[ -z "$charon" ]] || kill "$charon" 2>/dev/null || true; wait "$runner" || true'\'' TERM INT
                wait "$runner"
            ' _ "$binary" > "$path/$role.log" 2>&1 &
            # Environment is scoped to each endpoint, without modifying the host.
            pids+=("$!")
        done
        failed=0
        for process in "${pids[@]}"; do wait "$process" || failed=1; done
        pids=()
        if [[ "$failed" == 1 || ! -f "$path/passed" ]]; then
            cat "$path"/*.log
            detail=$(grep -hE 'real kernel Connection Test failed:|cancellation did not cleanly|\.go:[0-9]+:' "$path"/*.log | tail -n 1 || true)
            detail=${detail//'%'/'%25'}
            echo "::error title=Kernel Connection Test::$method $mode: $detail"
            exit 1
        fi
        for namespace in "$first" "$second"; do
            ip -n "$namespace" -j link show | python3 -c 'import json,sys; assert not any(x.get("ifalias","").startswith("backpack-kernel:") for x in json.load(sys.stdin)), "kernel test leaked an interface"'
        done
        echo "PASS: actual Connection Test $method $mode (4 echoes, verified four-stream download/upload, peer config retained, cleanup)"
    done
done
