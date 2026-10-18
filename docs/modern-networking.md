# Network efficiency and modern carriers

These additions build on v1.12.0. Use the same new build on both peers before
pairing advanced settings. Existing configurations retain their old defaults.

## Automatic Economy and panel tools

Automatic offers Economy (recommended) and Full. Economy creates at most three
eligible carriers, preferring Hysteria2 and independent TCP implementations.
Both peers run only these carriers. Its paired client probes 1 KiB in each
direction every five seconds on selected/unhealthy paths, and every sixty
seconds on healthy unselected backups. Promotion wakes verification immediately.
The smaller sample reduces overhead but is not a capacity benchmark. Reduced
backup freshness is a deliberate tradeoff; a backup may fail between probes.
Active TCP forwards keep their connections. Local helper exits withdraw paths
immediately; unverified routes never become a fallback.

```toml
[client.path_policy]
probe_bytes = 1024
probe_interval = 5
standby_interval = 60
```

Metrics show stream probe bytes, attempts, cadence, helper-tree RSS and cumulative
CPU seconds. Probe counters include framing and partial failures, exclude outer
TLS/QUIC and native UDP verification, and reset on engine restart. RSS includes
shared pages per process. Helper CPU counters reset after helper restart;
they are not CPU utilization. Failed samples and in-process carriers are unmeasured.

Open **Connection Test → Saved tests, Automatic setup and diagnostics** in the
panel. Automatic previews reserve listener ports for two minutes without creating
files or services. Review the required TCP/UDP ports, then create and copy the
private Setup Link to Kharej. In Network tools on Kharej, use Pair from a Setup
Link, review the peer settings, then create it. Existing peers retain their
reviewed edit flow. Optional REALITY/ShadowTLS covers are verified before
inclusion. Firewall rules remain under operator control. A competing process can
still win a port between reservation release and helper startup.

Browser tests now use the same private, bounded history and comparison rules as
terminal tests. Cancelled runs remain partial. History-write errors do not change
measurement verdicts. Carrier diagnosis uses temporary helpers on Kharej without
restarting its active tunnel. Read tokens cannot create tunnels or start helpers.

## Gecko, ECH and Hysteria Realm

Use pinned sing-box v1.14.2. Managed Hysteria2 supports `obfs_type = "gecko"`.
An existing `obfs_password` without a type still means Salamander. Optional
`gecko_min` / `gecko_max` default to 512 / 1200, with bounds
512 <= min <= max <= 1400. Both peers must agree. Hysteria2 also accepts
`bbr_profile = "standard"`, `"conservative"`, or `"aggressive"`; the existing
standard default is retained. Compare sustained uploads/downloads before tuning.

ECH is optional on managed Hysteria2, AnyTLS and TUIC. Enable it during server
setup and provide its public DNS name. Backpack runs the pinned helper's ECH
generator with bounded time/output and retains the private `ech_key` (ECH KEYS
PEM) on the server. Only public `ech_config` (ECH CONFIGS PEM) enters the Setup
Link. TLS certificate, chain and hostname verification remain enabled. ECH hides
the inner ClientHello; the destination IP and outer public name remain visible.
Rotate the server pair and reapply its link when changing ECH configuration.

For an existing Realm rendezvous, configure both peers:

```toml
[server.sing.realm] # [client.sing.realm] on Kharej
server_url = "https://realm.example.com:443"
token = "INDEPENDENT_REALM_ACCOUNT_TOKEN_AT_LEAST_16_BYTES"
realm_id = "backpack-pair-one"
stun_servers = "stun1.example.com:3478,stun2.example.com:3478"
ip_version = 4
```

Realm is Hysteria2-only and requires explicit STUN endpoints and its own account
token. The client discovers its remote UDP endpoint through Realm. Diagnostics
skip misleading direct QUIC probes and verify the actual helper's authenticated
payload path instead. The reverse destination stays restricted. NAT type and UDP
reachability still determine whether traversal works. Realm HTTPS uses system
trust; private deployments must provision their public CA in the helper's trust
environment. TLS verification cannot be disabled.

### Hosting a rendezvous service

Create a separate mode-0600 TOML configuration on a public rendezvous server:

```toml
[realm]
binary = "/opt/backpack/helpers/sing-box"
listen = "0.0.0.0:443"
server_name = "realm.example.com"
certificate = "/etc/letsencrypt/live/realm.example.com/fullchain.pem"
key = "/etc/letsencrypt/live/realm.example.com/privkey.pem"
token = "INDEPENDENT_REALM_ACCOUNT_TOKEN_AT_LEAST_16_BYTES"
max_realms = 16
```

```sh
chmod 600 /etc/backpack/realm.toml
backpack realm check -c /etc/backpack/realm.toml
backpack realm run -c /etc/backpack/realm.toml
```

The foreground process owns the helper and handles SIGTERM, restart and cleanup.
For systemd use a separate unit with
`ExecStart=/usr/local/bin/backpack realm run -c /etc/backpack/realm.toml`,
`Restart=on-failure`, and `KillMode=control-group`. Renew TLS certificates
externally and restart the service. This is a bounded rendezvous service, not a
traffic relay. Its private readiness listener rejects outbound proxy requests.

## Native CONNECT-IP (RFC 9484)

Choose Direct → HTTPS → CONNECT-IP or configure `carrier = "connect-ip"` under
`[l3]`. Pinned connect-ip-go v0.6.0 carries complete original IPv4/IPv6 packets
over verified HTTP/3 Datagrams. It inserts no GRE, Noise or invented IP envelope.
TLS authenticates the server; CONNECT authorization derives an independent
credential from the paired token. This differs from existing CONNECT-UDP modes.

```toml
[l3]
mode = "listen" # Kharej; use dial and Kharej's endpoint on Iran
addr = "0.0.0.0:443"
carrier = "connect-ip"
token = "LONG_RANDOM_PAIRED_TUNNEL_TOKEN"
local_ip = "10.205.0.2/30" # Iran: 10.205.0.1/30
peer_ip = "10.205.0.1"    # Iran: 10.205.0.2
iface = "bp0"
mtu = 1280
auto_mtu = false
masque_cert = "/etc/backpack/tls/server.pem" # listener only
masque_key = "/etc/backpack/tls/server.key"  # listener only
masque_server_name = "kharej.example.com"
# Dialler only, for a private CA:
# masque_ca = "/etc/backpack/tls/public-ca.pem"
```

Each session assigns exactly its configured peer address and advertises only its
own host address. Packets are checked against these peers. Capsule announcements
never install DNS, default routes, NAT or arbitrary remote routes. One authorized
session owns the TUN; unverified peers cannot evict it and have bounded setup
lifetimes. TUN traffic/ICMP establishes liveness; a TLS handshake alone is not
health. ICMP Packet Too Big feedback is returned to the local TUN.

This paired point-to-point mode uses MTU 1280 and rejects Noise auto-MTU, GRE keys,
FEC and multipath. General remote-access routing and external proxy authentication
schemes are outside this mode. HTTP/3 requires UDP; it does not silently switch to
TCP. Existing L3 forwarding and service management remain available. The outer
path must accommodate QUIC overhead in addition to the inner packet.

## Sustained measurement

Use an echo backend for availability/recovery sampling:

```sh
tunnelbench --serve 127.0.0.1:9001
tunnelbench --mode soak --target hy2=IRAN_FORWARD:PORT --parallel 1 \
  --duration 60m --io-timeout 2s --chunk 1024 --pid hy2=ENGINE_PID --json
```

Each fresh connection verifies random bytes. JSON records attempted/received
probes, unanswered-probe percentage, p95 RTT, successive-success jitter, outages,
observed recovery and completion. Cancellation emits partial JSON and a failing
exit code. Probe cadence and timeout contribute to recovery time; these figures
are not TCP packet loss, capacity or an uptime guarantee.

For sustained independent directions use `--mode bulk` on backend and client,
for example `--duration 30m --parallel 4 --json`. Each direction gets its own
measurement window. Receiver-confirmed uploads, nonrepeating verification and
bounded cancellation are retained. Bulk stops on a failed stream; soak measures
reconnection. Tests in local namespaces do not establish results for Iranian ISPs.

For reproducible sustained helper validation, manually dispatch **Protocol suite**
with `sustained=true`. Separate disposable runners observe Hysteria2 and TUIC for
thirty minutes, verify independent bulk directions, inject a helper restart at
minute fifteen, and upload credential-free measurements. Resource roots cover
both engine process trees; the echo/bulk backends are separate processes. This
uses runner-local networking and must not be described as an Iran route result.

<div dir="rtl">

## خلاصهٔ فارسی

حالت Economy حداکثر سه حامل می‌سازد و تست مسیر پشتیبان سالم را کم‌تکرارتر می‌کند؛
مصرف واقعی تست و منابع helper در آمار دیده می‌شود. در Network tools پنل، تونل
Automatic را ابتدا با فهرست پورت‌ها بازبینی و سپس ایجاد کنید؛ سمت خارج لینک را
بازبینی و جفت‌سازی می‌کند. تاریخچهٔ تست‌های پنل و ترمینال مشترک است و عیب‌یابی
مرحله‌ای، تونل فعال را ری‌استارت نمی‌کند.

Gecko برای Hysteria2 و ECH اختیاری برای Hysteria2، AnyTLS و TUIC اضافه شده است.
کلید خصوصی ECH فقط روی سرور می‌ماند؛ لینک فقط تنظیم عمومی را می‌برد. Realm برای
کشف همتا پشت NAT است و به حساب مستقل، STUN و مسیر UDP سالم نیاز دارد. سرویس
مستقل آن با فرمان `backpack realm run -c FILE` اجرا می‌شود. CONNECT-IP بستهٔ
کامل IP را روی HTTP/3 می‌برد، فقط آدرس همتای تنظیم‌شده را می‌پذیرد و مسیر پیش‌فرض
یا DNS میزبان را تغییر نمی‌دهد. هر دو سر باید نسخهٔ دارای این قابلیت‌ها را داشته
باشند. تست یک‌ساعته و نتایج CI جای اندازه‌گیری مسیر واقعی اپراتورها را نمی‌گیرند.

</div>

*Last verified against Backpack v1.12.0 with this PR's additions, 2026-10-18.*
