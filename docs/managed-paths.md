# Verified managed paths

Requires BackPack v1.11.0 or later on **both** machines. With v1.12.0, choose
[**Automatic**](automatic-setup.md) beside Reverse and Direct for generated
settings, then use **Setup Kharej → Setup Link** on the other machine. The detailed
editor remains under **Reverse → HTTPS → Adaptive Managed Paths**. Configure two
to eight independent public carriers on Iran and export its Setup Link to Kharej.
Names identify paths in metrics and must be unique ASCII identities of at most
32 characters. Public listeners must not overlap. An optional `endpoint` exports
a different public host:port when the listener uses a wildcard or a frontend.
All helpers reach one literal loopback Reverse target with one token.

The existing Reverse engine owns the forwarded ports once. Helpers stay warm;
authenticated probes upload and download known bytes through each actual
carrier without claiming the Reverse control channel or opening a backend.
Local helper readiness or a successful TLS handshake alone never passes a
probe. RTT, measured transfer rate and recent probe failures contribute to an
EWMA score. This is a small verified-transfer sample, not a capacity benchmark.

Selection affects **new** TCP tunnel connections. Idle pool connections on the
old path close and refill through the new path. Active forwards retain their
original connections. A failed active connection cannot be migrated; its
application must reconnect. There is a narrow destination-assignment boundary
while a pooled connection becomes active, so this is not an atomic migration
protocol. If every candidate fails verification, new connections fail closed
until one recovers. No unverified direct connection is substituted.
After a hard outage, pool refill and control-channel recovery can outlast the
selection decision; applications should retry failed connection attempts.

Native UDP selection is separate and includes only Hysteria2/TUIC paths with
`native_udp = true`. Each candidate must also pass an authenticated datagram
probe. One Noise association and bounded flow table survive changes of the
local helper association. Outage packets are dropped rather than queued or
replayed. A Short HTTPS or another stream-only carrier is never substituted for
native UDP. When native UDP is enabled, non-native paths carry only TCP.

## Configuration

This minimal TCP-only example uses two Short HTTPS endpoints; any supported
managed helper can occupy either path. Static certificates must cover their
configured names. The wizard can generate private certificates and the Setup
Link transfers only public trust. Keep configuration and Setup Links private.

```toml
# Iran
[server]
bind_addr = "127.0.0.1:3080"
transport = "tcp"
token = "REPLACE-WITH-A-LONG-RANDOM-REVERSE-TOKEN"
accept_udp = false
ports = ["8080=127.0.0.1:8081"]

[[server.paths]]
name = "primary"
endpoint = "first.example.com:443"
[server.paths.short]
listen = "0.0.0.0:8443"
server_name = "first.example.com"
certificate = "/etc/backpack/tls/first.pem"
key = "/etc/backpack/tls/first.key"

[[server.paths]]
name = "backup"
endpoint = "second.example.com:9443"
[server.paths.short]
listen = "0.0.0.0:9443"
server_name = "second.example.com"
certificate = "/etc/backpack/tls/second.pem"
key = "/etc/backpack/tls/second.key"
```

```toml
# Kharej
[client]
remote_addr = "127.0.0.1:3080"
transport = "tcp"
token = "REPLACE-WITH-THE-SAME-REVERSE-TOKEN"
connection_pool = 8

[client.path_policy]
probe_interval = 5
probe_timeout = 5
probe_bytes = 16384
failures = 2
recoveries = 2
min_dwell = 30
improvement = 20

[[client.paths]]
name = "primary"
[client.paths.short]
server = "first.example.com:443"
server_name = "first.example.com"
# ca_file = "/etc/backpack/tls/first-public-ca.pem"

[[client.paths]]
name = "backup"
[client.paths.short]
server = "second.example.com:9443"
server_name = "second.example.com"
# ca_file = "/etc/backpack/tls/second-public-ca.pem"
```

Each path sets exactly one `naive`, `xray`, `sing` or `short` table, using the
same keys as the corresponding inline helper. Binary paths and private TLS
keys stay local. Native paths in one process must use identical local UDP
limits. An inline helper, old address/transport fallback chain, MPTCP, routing
override, Direct or L3 cannot be combined with this profile.

Zero policy values select the defaults above. Probe interval accepts 1..300
seconds, timeout 1..30 seconds, payload 1..64 KiB per direction, failure/recovery
thresholds 1..10, healthy dwell 1..3600 seconds, and improvement 1..90 percent.
A first verified success makes a new path eligible. Failed paths need the
recovery threshold. Healthy switches require both dwell and the improvement
margin; withdrawal of a failed path bypasses healthy dwell. Default probes
carry 32 KiB per path plus framing every five seconds, with extra TLS/QUIC
setup cost. Short HTTPS especially benefits from modest pools and measured
probe settings.

Normal edits and Setup Link updates preserve path settings and local policy.
Each helper restarts independently under the tunnel context. A reported local
helper exit withdraws its TCP and native UDP path immediately, bypassing remote
probe failure thresholds and healthy dwell. Idle connections are retired; active
forwards retain ownership. Readiness after restart wakes verification promptly,
and the recovery threshold still applies. Late results from the previous helper
process cannot revive the restarted path. Valid static TLS
replacement triggers the normal generation reload; such reloads may interrupt
active flows. Metrics show selected TCP/native UDP paths and per-path verified
RTT, transfer rate, probe failures and helper readiness in the CLI and panel.
These contain path identities and measurements, never credentials.

Test real forwarded traffic, keep a long-lived application connection open,
degrade one candidate, then block it. New connections should recover through a
verified survivor while still-working active connections retain their path.
Automated Linux tests exercise this behavior; your actual ISP, frontends and
certificate renewal still require validation in your deployment.

<div dir="rtl">

## خلاصهٔ فارسی

در منوی HTTPS، گزینهٔ Managed adaptive paths دو تا هشت مسیر مستقل را گرم نگه
می‌دارد. انتخاب بر اساس انتقال واقعی و تأییدشده است. اتصال‌های تازه از مسیر
منتخب می‌روند و اتصال فعال روی مسیر قبلی می‌ماند؛ اتصال قطع‌شده قابل مهاجرت
نیست. UDP بومی فقط از Hysteria2/TUIC تأییدشده عبور می‌کند و به جریان مطمئن
تبدیل نمی‌شود. لینک جفت‌سازی را روی ایران بساز و روی خارج وارد کن. هر دو سر
باید همین نسخه را داشته باشند.

</div>

*Last verified against Backpack v1.14.2, 2026-10-18.*
