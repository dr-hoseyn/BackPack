#!/usr/bin/env bash
# Run under: sudo env TESTBIN=... W=... unshare --net --mount --fork bash ...
set -euo pipefail
: "${TESTBIN:?absolute network test binary}" "${W:?private temporary state directory}"
mount --make-rprivate /
mkdir -p /run/netns "$W"
mount -t tmpfs tmpfs /run/netns
cleanup() {
  for ns in bpclient bpserver; do
    ip netns pids "$ns" 2>/dev/null | xargs -r kill 2>/dev/null || true
    ip netns del "$ns" 2>/dev/null || true
  done
}
trap cleanup EXIT
ip netns add bpclient
ip netns add bpserver
for path in 1 2; do
  ip link add "c$path" type veth peer name "s$path"
  ip link set "c$path" netns bpclient
  ip link set "s$path" netns bpserver
  subnet=$((220 + path))
  ip -n bpclient addr add "10.$subnet.0.1/24" dev "c$path"
  ip -n bpserver addr add "10.$subnet.0.2/24" dev "s$path"
  ip -n bpclient link set "c$path" up
  ip -n bpserver link set "s$path" up
done
for ns in bpclient bpserver; do
  ip -n "$ns" link set lo up
  ip netns exec "$ns" sysctl -qw net.mptcp.enabled=1
  ip netns exec "$ns" ip mptcp limits set subflows 4 add_addr_accepted 4
done
ip netns exec bpclient ip mptcp endpoint add 10.222.0.1 dev c2 subflow fullmesh
ip netns exec bpserver ip mptcp endpoint add 10.222.0.2 dev s2 signal
ip netns exec bpserver env BP_MPTCP_ROLE=server BP_MPTCP_STATE="$W" "$TESTBIN" -test.run '^TestMPTCPLiveNamespacePeer$' -test.v >"$W/server.log" 2>&1 &
server=$!
for i in {1..100}; do [[ -f "$W/ready" ]] && break; sleep .1; done
test -f "$W/ready" || { cat "$W/server.log"; exit 1; }
ip netns exec bpclient env BP_MPTCP_ROLE=client BP_MPTCP_STATE="$W" "$TESTBIN" -test.run '^TestMPTCPLiveNamespacePeer$' -test.v >"$W/client.log" 2>&1 &
client=$!
subflows=0
for i in {1..100}; do
  subflows=$(ip netns exec bpclient ss -tnH state established '( dport = :45000 )' | wc -l)
  [[ -f "$W/negotiated" && "$subflows" -ge 2 ]] && break
  sleep .1
done
ip netns exec bpclient ss -tni '( dport = :45000 )'
[[ "$subflows" -ge 2 ]] || { cat "$W/client.log"; echo 'No independent second subflow'; exit 1; }
ip -n bpclient link set c1 down
touch "$W/primary-down"
if ! wait "$client"; then cat "$W/client.log" "$W/server.log"; exit 1; fi
if ! wait "$server"; then cat "$W/server.log"; exit 1; fi
cat "$W/client.log" "$W/server.log"
echo 'RESULT OK: negotiated MPTCP, multiple subflows, same connection survived primary-path outage'
