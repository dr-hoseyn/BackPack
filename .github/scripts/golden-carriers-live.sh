#!/usr/bin/env bash
set -euo pipefail

binary="$(realpath "$1")"
work="$(mktemp -d /tmp/backpack-golden-live-XXXXXX)"
first="bpga-$$"; second="bpgb-$$"
pids=(); echo_pid=""
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
ip netns add "$first"; ip netns add "$second"
ip link add bpga-veth type veth peer name bpgb-veth
ip link set bpga-veth netns "$first"; ip link set bpgb-veth netns "$second"
for pair in "$first 1 bpga-veth" "$second 2 bpgb-veth"; do
    read -r namespace host device <<< "$pair"
    ip -n "$namespace" link set lo up
    ip -n "$namespace" addr add "192.0.2.$host/24" dev "$device"
    ip -n "$namespace" -6 addr add "2001:db8:1::$host/64" dev "$device" nodad
    ip -n "$namespace" link set "$device" up
done
ip netns exec "$second" python3 -u - > "$work/echo.log" 2>&1 <<'PY_ECHO' &
import socket,threading
def peer(c):
    with c:
        while True:
            b=c.recv(65536)
            if not b:return
            c.sendall(b)
def tcp():
    s=socket.socket();s.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1);s.bind(('0.0.0.0',30222));s.listen()
    while True:
        c,_=s.accept();threading.Thread(target=peer,args=(c,),daemon=True).start()
threading.Thread(target=tcp,daemon=True).start()
s=socket.socket(socket.AF_INET,socket.SOCK_DGRAM);s.bind(('0.0.0.0',30222))
while True:
    b,p=s.recvfrom(65536);s.sendto(b,p)
PY_ECHO
echo_pid=$!

family="${FAMILY:-ipv4}"
for carrier in dns icmp-request icmp-reply; do
    path="$work/$family-$carrier"; mkdir -p "$path"
    for side in 1 2; do
        peer=$((3-side)); namespace="$first"; mode=dial
        [[ "$side" == 2 ]] && { namespace="$second"; mode=listen; }
        local_addr="192.0.2.$side"; peer_addr="192.0.2.$peer"; addr="$peer_addr:30530"
        [[ "$mode" == listen ]] && addr="$local_addr:30530"
        if [[ "$family" == ipv6 ]]; then
            local_addr="2001:db8:1::$side"; peer_addr="2001:db8:1::$peer"; addr="[$peer_addr]:30530"
            [[ "$mode" == listen ]] && addr="[$local_addr]:30530"
        fi
        cat > "$path/$side.toml" <<EOF
[l3]
mode = "$mode"
addr = "$addr"
carrier = "$carrier"
token = "isolated-golden-carrier-noise-secret-2026"
local_ip = "10.78.1.$side/30"
peer_ip = "10.78.1.$peer"
iface = "bpggtest"
mtu = 1280
auto_mtu = false
fec_data = 2
fec_parity = 1
EOF
        if [[ "$side" == 1 ]]; then
            printf '\nports = ["127.0.0.1:30221=30222"]\naccept_udp = true\n' >> "$path/$side.toml"
        fi
        if [[ "$carrier" == dns ]]; then
            printf '\ndns_bind = "%s"\ndns_rotate = 1\ndns_rate = 1000\npaths = 2\n' "$local_addr" >> "$path/$side.toml"
        fi
        ip netns exec "$namespace" "$binary" -c "$path/$side.toml" > "$path/$side.log" 2>&1 & pids+=("$!")
    done
    passed=false
    for attempt in $(seq 1 30); do
        if ip netns exec "$first" ping -n -c 1 -W 1 10.78.1.2 > "$path/ping.log" 2>&1; then passed=true; break; fi
        sleep .2
    done
    if [[ "$passed" == false ]]; then cat "$path"/*.log; exit 1; fi
    # This feature must not disable ordinary host ping to suppress carrier copies.
    public_peer="192.0.2.2"; [[ "$family" == ipv6 ]] && public_peer="2001:db8:1::2"
    ip netns exec "$first" ping -n -c 1 -W 2 "$public_peer" >> "$path/ping.log"
    ip netns exec "$first" ping -n -c 2 -W 2 -s 1200 10.78.1.2 >> "$path/ping.log"
    ip netns exec "$second" ping -n -c 2 -W 2 10.78.1.1 >> "$path/ping.log"
    ip netns exec "$first" python3 - <<'PY_TRAFFIC'
import os,socket,time
payload=os.urandom(256*1024)
for attempt in range(30):
    try:
        with socket.create_connection(('127.0.0.1',30221),timeout=3) as c:
            c.settimeout(15);c.sendall(payload);got=b''
            while len(got)<len(payload):
                block=c.recv(len(payload)-len(got))
                if not block:raise RuntimeError('forwarded TCP response ended early')
                got+=block
            assert got==payload
        break
    except OSError:
        if attempt==29:raise
        time.sleep(.1)
with socket.socket(socket.AF_INET,socket.SOCK_DGRAM) as c:
    c.settimeout(5);payload=os.urandom(768);c.sendto(payload,('127.0.0.1',30221));got,_=c.recvfrom(2048);assert got==payload
print('PASS: verified TCP and UDP payloads')
PY_TRAFFIC
    stop_pair
    if ip -n "$first" link show bpggtest >/dev/null 2>&1 || ip -n "$second" link show bpggtest >/dev/null 2>&1; then
        echo "FAILED: $carrier leaked a tunnel device" >&2; exit 1
    fi
    echo "PASS: $carrier $family (Noise, FEC, TCP/UDP, ping, MTU and teardown)"
done
