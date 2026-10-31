#!/usr/bin/env bash
set -euo pipefail

# Only disposable namespaces and files created by this script are touched.
binary="$(realpath "$1")"
work="$(mktemp -d /tmp/backpack-kernel-live-XXXXXX)"
first="bpka-$$"
second="bpkb-$$"
pids=()
echo_pid=""
stop_pair() {
    local process
    for process in "${pids[@]}"; do kill -TERM "$process" 2>/dev/null || true; done
    for process in "${pids[@]}"; do wait "$process" 2>/dev/null || true; done
    pids=()
}
cleanup() {
    stop_pair
    if [[ -n "$echo_pid" ]]; then kill -TERM "$echo_pid" 2>/dev/null || true; wait "$echo_pid" 2>/dev/null || true; fi
    ip netns del "$first" 2>/dev/null || true
    ip netns del "$second" 2>/dev/null || true
    if [[ -n "${SUDO_UID:-}" ]]; then chown -R "$SUDO_UID:${SUDO_GID:-$SUDO_UID}" "$work"; fi
}
trap cleanup EXIT
ip netns add "$first"
ip netns add "$second"
ip link add bpka-veth type veth peer name bpkb-veth
ip link set bpka-veth netns "$first"
ip link set bpkb-veth netns "$second"
for pair in "$first 1 bpka-veth" "$second 2 bpkb-veth"; do
    read -r namespace host device <<< "$pair"
    ip -n "$namespace" link set lo up
    ip -n "$namespace" addr add "192.0.2.$host/24" dev "$device"
    ip -n "$namespace" -6 addr add "2001:db8:1::$host/64" dev "$device" nodad
    ip -n "$namespace" link set "$device" up
done

ip netns exec "$second" python3 -u - > "$work/echo.log" 2>&1 <<'PY_ECHO' &
import socket, threading
def tcp_peer(conn):
    with conn:
        while True:
            data=conn.recv(65536)
            if not data:return
            conn.sendall(data)
def tcp():
    s=socket.socket();s.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1);s.bind(('0.0.0.0',30222));s.listen()
    while True:
        conn,_=s.accept();threading.Thread(target=tcp_peer,args=(conn,),daemon=True).start()
threading.Thread(target=tcp,daemon=True).start()
s=socket.socket(socket.AF_INET,socket.SOCK_DGRAM);s.bind(('0.0.0.0',30222))
while True:
    data,peer=s.recvfrom(65536);s.sendto(data,peer)
PY_ECHO
echo_pid=$!

for mode in direct reverse; do
    for method in gre ipip 6to4 6to4-gre sit ip6gre ipip6 ip6ip6 wireguard xfrm-ipsec fou-gre gue-ipip ecmp-lanes eoip gretap ip6gretap vxlan geneve l2tpv3 etherip; do
        path="$work/$mode-$method"
        mkdir -p "$path"
        rev=false
        [[ "$mode" == reverse ]] && rev=true
        inner4=true
        [[ "$method" == sit || "$method" == ip6ip6 ]] && inner4=false
        for side in 1 2; do
            peer=$((3-side))
            role=iran
            [[ "$side" == 2 ]] && role=kharej
            local_addr="192.0.2.$side"; remote_addr="192.0.2.$peer"
            case "$method" in ip6gre|ipip6|ip6ip6|ip6gretap) local_addr="2001:db8:1::$side"; remote_addr="2001:db8:1::$peer" ;; esac
            local_ip="10.77.7.$side/30"; peer_ip="10.77.7.$peer"
            if [[ "$inner4" == false ]]; then local_ip="fd77:7::$side/64"; peer_ip="fd77:7::$peer"; fi
            cat > "$path/$side.toml" <<EOF
[kernel]
method = "$method"
role = "$role"
local_addr = "$local_addr"
remote_addr = "$remote_addr"
local_ip = "$local_ip"
peer_ip = "$peer_ip"
interface = "bpktest"
token = "isolated-kernel-integration-secret-2026"
id = 321
mtu = 1280
reverse = $rev
EOF
            if [[ "$side" == 1 && "$inner4" == true ]]; then
                printf '\nports = ["127.0.0.1:30221=30222"]\naccept_udp = true\n' >> "$path/$side.toml"
            fi
            namespace="$first"; [[ "$side" == 2 ]] && namespace="$second"
            if [[ "$method" == xfrm-ipsec ]]; then
                ip netns exec "$namespace" unshare --mount --propagation private bash -c '
                    mount -t tmpfs tmpfs /run
                    /usr/lib/ipsec/charon > "$3/charon-$4.log" 2>&1 &
                    charon=$!
                    trap '\''kill "$charon" 2>/dev/null || true'\'' EXIT
                    for attempt in $(seq 1 50); do [[ -S /run/charon.vici ]] && break; sleep .1; done
                    "$1" -c "$2" & engine=$!
                    trap '\''kill "$engine" "$charon" 2>/dev/null || true; wait "$engine" || true'\'' TERM INT
                    wait "$engine"
                ' _ "$binary" "$path/$side.toml" "$path" "$side" > "$path/$side.log" 2>&1 &
            else
                ip netns exec "$namespace" "$binary" -c "$path/$side.toml" > "$path/$side.log" 2>&1 &
            fi
            pids+=("$!")
        done
        target="10.77.7.2"; [[ "$inner4" == false ]] && target="fd77:7::2"
        passed=false
        for attempt in $(seq 1 25); do
            if ip netns exec "$first" ping -n -c 1 -W 1 "$target" > "$path/ping.log" 2>&1; then passed=true; break; fi
            sleep .2
        done
        if [[ "$passed" == false ]]; then
            cat "$path"/*.log
            echo "FAILED: $method $mode" >&2
            exit 1
        fi
        # Check full MTU payloads and the reverse direction after idle.
        sleep 1
        ip netns exec "$first" ping -n -c 2 -W 2 -s 1200 "$target" >> "$path/ping.log"
        target="10.77.7.1"; [[ "$inner4" == false ]] && target="fd77:7::1"
        ip netns exec "$second" ping -n -c 2 -W 2 "$target" >> "$path/ping.log"
        if [[ "$inner4" == true ]]; then
            ip netns exec "$first" python3 - <<'PY_CLIENT'
import os,socket,time
payload=os.urandom(65536)
for attempt in range(30):
    try:
        with socket.create_connection(('127.0.0.1',30221),timeout=2) as c:
            c.sendall(payload);data=b''
            while len(data)<len(payload):
                part=c.recv(len(payload)-len(data))
                if not part:raise RuntimeError('TCP forwarding closed before the verified response')
                data+=part
            assert data==payload
        break
    except OSError:
        if attempt==29:raise
        time.sleep(.1)
with socket.socket(socket.AF_INET,socket.SOCK_DGRAM) as c:
    c.settimeout(3);payload=os.urandom(768);c.sendto(payload,('127.0.0.1',30221));data,_=c.recvfrom(2048);assert data==payload
print('PASS: verified TCP and UDP forwarding')
PY_CLIENT
        fi
        stop_pair
        if ip -n "$first" link show bpktest >/dev/null 2>&1 || ip -n "$second" link show bpktest >/dev/null 2>&1; then
            echo "FAILED: $method $mode leaked its interface" >&2
            exit 1
        fi
        echo "PASS: $method $mode (bidirectional traffic, MTU, teardown)"
    done
done
