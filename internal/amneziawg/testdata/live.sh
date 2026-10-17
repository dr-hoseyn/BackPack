#!/bin/bash
# Run only inside a disposable network AND mount namespace, as in CI below.
set -euo pipefail
: "${BIN:?}" "${W:?}"
umask 077
mkdir -p "$W"
mount --make-rprivate /
mount -t tmpfs none /run
mkdir -p /run/netns
server_pid= client_pid=
cleanup() {
  for pid in "$server_pid" "$client_pid"; do
    if [ -n "$pid" ]; then kill "$pid" 2>/dev/null || true; wait "$pid" 2>/dev/null || true; fi
  done
  for ns in awg-server awg-client; do ip netns del "$ns" 2>/dev/null || true; done
}
trap cleanup EXIT
ip netns add awg-server
ip netns add awg-client
ip link add awgs type veth peer name awgc
ip link set awgs netns awg-server
ip link set awgc netns awg-client
endpoint=10.99.17.1:25999
server_ip=10.254.248.1
client_ip=10.254.248.2
inner=(--address "$server_ip/32" --peer-address "$client_ip/32")
if [ "${IPV6:-}" = 1 ]; then
  ip -n awg-server -6 addr add fd99:17::1/64 dev awgs nodad
  ip -n awg-client -6 addr add fd99:17::2/64 dev awgc nodad
  endpoint='[fd99:17::1]:25999'
  server_ip=fd00:254:248::1
  client_ip=fd00:254:248::2
  inner=(--address "$server_ip/128" --peer-address "$client_ip/128")
else
  ip -n awg-server addr add 10.99.17.1/24 dev awgs
  ip -n awg-client addr add 10.99.17.2/24 dev awgc
fi
for ns in awg-server awg-client; do ip -n "$ns" link set lo up; done
ip -n awg-server link set awgs up
ip -n awg-client link set awgc up
if [ -n "${NETEM:-}" ]; then
  # NETEM is supplied only by the test runner, never by a configuration file.
  ip netns exec awg-server tc qdisc add dev awgs root netem $NETEM
  ip netns exec awg-client tc qdisc add dev awgc root netem $NETEM
fi
"$BIN" awg init --endpoint "$endpoint" --dir "$W/pair" "${inner[@]}"
start_server() {
  ip netns exec awg-server "$BIN" awg run --config "$W/pair/server.json" >> "$W/server.log" 2>&1 &
  server_pid=$!
}
start_server
ip netns exec awg-client "$BIN" awg run --config "$W/pair/client.json" > "$W/client.log" 2>&1 &
client_pid=$!
await_ping() {
  for ((i=0;i<${1};i++)); do
    kill -0 "$server_pid" "$client_pid" || { cat "$W/server.log" "$W/client.log"; return 1; }
    if ip netns exec awg-client ping -c1 -W1 "$server_ip" >/dev/null 2>&1; then return 0; fi
    sleep .2
  done
  return 1
}
await_ping 30
head -c 1048576 /dev/urandom > "$W/payload"
copy() {
  local from=$1 to=$2 destination=$3 port=$4
  ip netns exec "$to" timeout 20 nc -l -p "$port" > "$W/received" &
  local receiver=$!
  sleep .2
  ip netns exec "$from" timeout 20 nc -N -w 15 "$destination" "$port" < "$W/payload"
  wait "$receiver"
  cmp "$W/payload" "$W/received"
}
copy awg-client awg-server "$server_ip" 26001
copy awg-server awg-client "$client_ip" 26002
# A second process must refuse the live interface without changing it.
if ip netns exec awg-server "$BIN" awg run --config "$W/pair/server.json" > "$W/duplicate.log" 2>&1; then
  echo 'duplicate interface unexpectedly accepted'; exit 1
fi
await_ping 3
if [ "${RESTART:-}" = 1 ]; then
  kill -TERM "$server_pid"; wait "$server_pid"; server_pid=
  if ip -n awg-server link show bpawg0 >/dev/null 2>&1; then echo 'interface leaked after shutdown'; exit 1; fi
  start_server
  # AWG retains normal WireGuard rekey behavior. A restarted peer may need
  # the next handshake; do not misrepresent process readiness as recovery.
  await_ping 160
  copy awg-client awg-server "$server_ip" 26003
fi
kill -TERM "$client_pid" "$server_pid"
wait "$client_pid"; client_pid=
wait "$server_pid"; server_pid=
for ns in awg-server awg-client; do
  if ip -n "$ns" link show bpawg0 >/dev/null 2>&1; then echo 'interface leaked'; exit 1; fi
done
echo 'RESULT OK AmneziaWG: bidirectional 1MiB checksums, exclusive interface, shutdown cleanup'
