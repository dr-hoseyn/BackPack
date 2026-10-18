#!/usr/bin/env bash
# Disposable CI runner only. Two actual Backpack processes, pinned helpers,
# verified bulk transfers and a 30 minute availability/recovery observation.
set -euo pipefail
task_protocol="$1"
case "$task_protocol" in hysteria2|tuic) ;; *) exit 2 ;; esac
: "${BP_SINGBOX_BINARY:?pinned helper required}"
: "${BP_LONG_ROOT:?private disposable output directory required}"
umask 077
mkdir -p "$BP_LONG_ROOT"
task_pids=()
cleanup() {
  trap - EXIT INT TERM
  for task_pid in "${task_pids[@]}"; do kill -TERM "$task_pid" 2>/dev/null || true; done
  for task_pid in "${task_pids[@]}"; do wait "$task_pid" 2>/dev/null || true; done
}
trap cleanup EXIT INT TERM
go build -o "$BP_LONG_ROOT/backpack" .
go build -o "$BP_LONG_ROOT/tunnelbench" ./tools/tunnelbench
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes \
  -keyout "$BP_LONG_ROOT/key.pem" -out "$BP_LONG_ROOT/cert.pem" -days 1 \
  -subj /CN=localhost -addext subjectAltName=DNS:localhost >/dev/null 2>&1
task_extra=''
if [ "$task_protocol" = tuic ]; then
  task_extra='uuid = "12345678-1234-4234-8234-123456789abc"'
else
  task_extra='obfs_password = "ci-only-salamander-account-secret"'
fi
cat > "$BP_LONG_ROOT/iran.toml" <<EOF
[server]
bind_addr = "127.0.0.1:30000"
transport = "tcp"
token = "ci-only-reverse-token-0123456789abcdef"
skip_optz = true
accept_udp = false
ports = ["127.0.0.1:30002=127.0.0.1:30003", "127.0.0.1:30004=127.0.0.1:30005"]
[server.sing]
binary = "$BP_SINGBOX_BINARY"
protocol = "$task_protocol"
listen = "127.0.0.1:30001"
server_name = "localhost"
password = "ci-only-independent-helper-account"
certificate = "$BP_LONG_ROOT/cert.pem"
key = "$BP_LONG_ROOT/key.pem"
$task_extra
EOF
cat > "$BP_LONG_ROOT/kharej.toml" <<EOF
[client]
remote_addr = "127.0.0.1:30000"
transport = "tcp"
token = "ci-only-reverse-token-0123456789abcdef"
skip_optz = true
connection_pool = 4
[client.sing]
binary = "$BP_SINGBOX_BINARY"
protocol = "$task_protocol"
server = "127.0.0.1:30001"
server_name = "localhost"
password = "ci-only-independent-helper-account"
ca_file = "$BP_LONG_ROOT/cert.pem"
$task_extra
EOF
"$BP_LONG_ROOT/tunnelbench" --serve 127.0.0.1:30003 > "$BP_LONG_ROOT/echo.log" 2>&1 &
task_pids+=("$!")
"$BP_LONG_ROOT/tunnelbench" --serve 127.0.0.1:30005 --mode bulk > "$BP_LONG_ROOT/bulk-server.log" 2>&1 &
task_pids+=("$!")
"$BP_LONG_ROOT/backpack" -c "$BP_LONG_ROOT/iran.toml" > "$BP_LONG_ROOT/iran.log" 2>&1 &
task_iran="$!"; task_pids+=("$task_iran")
"$BP_LONG_ROOT/backpack" -c "$BP_LONG_ROOT/kharej.toml" > "$BP_LONG_ROOT/kharej.log" 2>&1 &
task_kharej="$!"; task_pids+=("$task_kharej")
task_ready=false
for task_attempt in $(seq 1 30); do
  if "$BP_LONG_ROOT/tunnelbench" --target probe=127.0.0.1:30002 --parallel 1 --duration 1s --json > "$BP_LONG_ROOT/readiness.json" 2>/dev/null; then
    if python3 -c 'import json,sys; r=json.load(open(sys.argv[1]))[0];sys.exit(not (r["verified_bytes"]>0 and r["failures"]==0))' "$BP_LONG_ROOT/readiness.json"; then task_ready=true; break; fi
  fi
  sleep 1
done
[ "$task_ready" = true ] || { echo "Managed tunnel did not become ready"; exit 1; }
"$BP_LONG_ROOT/tunnelbench" --target "$task_protocol=127.0.0.1:30004" --mode bulk \
  --duration 15s --parallel 4 --io-timeout 3s --pid "$task_protocol=$task_iran" \
  --pid "$task_protocol=$task_kharej" --json > "$BP_LONG_ROOT/bulk.json"
# Fault only a currently owned helper, never a service or unrelated PID.
task_helper="$(pgrep -P "$task_kharej" -x sing-box | head -1)"
[ -n "$task_helper" ]
(
  sleep 900 &
  task_sleep="$!"
  trap 'kill -TERM "$task_sleep" 2>/dev/null || true; exit' TERM INT
  wait "$task_sleep"
  if [ "$(ps -o ppid= -p "$task_helper" | tr -d ' ')" = "$task_kharej" ]; then
    date -u +%FT%TZ > "$BP_LONG_ROOT/helper-fault-time.txt"
    kill -TERM "$task_helper"
  else
    echo "Initial helper no longer owned; intentional fault not injected" > "$BP_LONG_ROOT/fault-skipped.txt"
  fi
) &
task_pids+=("$!")
"$BP_LONG_ROOT/tunnelbench" --target "$task_protocol=127.0.0.1:30002" --mode soak \
  --duration 30m --parallel 1 --io-timeout 2s --chunk 1024 \
  --pid "$task_protocol=$task_iran" --pid "$task_protocol=$task_kharej" --json > "$BP_LONG_ROOT/soak.json"
python3 - "$BP_LONG_ROOT" <<'PY'
import json, pathlib, sys
p = pathlib.Path(sys.argv[1])
bulk = json.loads((p / 'bulk.json').read_text())
soak = json.loads((p / 'soak.json').read_text())[0]
assert len(bulk) == 2 and all(r['failures'] == 0 and r['verified_bytes'] > 0 for r in bulk)
s = soak['soak']
assert s['complete'] and soak['seconds'] >= 1800 and s['received'] >= 1700
assert s['unanswered_probe_percent'] <= 1 and s['max_observed_recovery_seconds'] <= 30
assert not s['outages'] or s['outages'][-1].get('end_utc')
assert (p / 'helper-fault-time.txt').exists(), 'intentional restart did not run'
print('RESULT OK: 30 minute verified observation and helper restart; this is a local runner, not an ISP result.')
PY
