# Short HTTPS

Requires BackPack v1.11.0 or later on both machines for managed mode.

*Last verified against Backpack v1.15.1, 2026-10-18.*

`backpack short-https` is an **experimental, standalone TCP port relay** for
paths that allow short HTTPS exchanges but stop established connections after
a few packets. It keeps a logical backend TCP connection while sending small
authenticated HTTPS requests over fresh physical connections. Byte offsets and
acknowledgements avoid duplicate delivery after a lost response. Logical EOF
preserves replies after the application closes its write direction.

It works with IP addresses and generated private IP certificates; no owned
domain, external helper or outbound proxy is needed. Certificate verification
remains enabled. The server permits only one configured literal loopback
backend. The client exposes that fixed backend on its configured local port.
The standalone command retains its disposable JSON workflow. Managed Reverse
setup integrates the relay with ordinary tunnel services and paired settings.

## Managed Reverse setup

Choose **Reverse → HTTPS → Short HTTPS** on Iran. Configure a public HTTPS
listener and a distinct literal loopback Reverse target. The wizard offers
static PEM files, a generated private IP/hostname certificate, or ACME for an
owned DNS name on TCP/443. The Setup Link carries the Reverse token, public
endpoint, verified server name and public trust anchor. The private key stays
on Iran. On Kharej choose the same protocol and paste the link.

The normal per-tunnel systemd service owns the relay and Reverse engine.
Ordinary edits preserve settings. Valid static certificate/key or public CA
replacement triggers the existing validated generation reload; incomplete or
invalid replacements keep the current generation. Reloads may interrupt flows.
ACME uses TLS-ALPN-01 and `autocert.Manager` for issuance and automatic renewal.
Its persistent private cache lives at
`/etc/backpack/acme/short-https/<domain>`. DNS must point to Iran and TCP/443
must reach this listener directly. Preserve the cache during ordinary restarts.
Local tests validate configuration/lifecycle; production ACME issuance and
renewal additionally require a reachable owned domain.

```toml
# Iran
[server]
bind_addr = "127.0.0.1:3080"
transport = "tcp"
token = "REPLACE-WITH-A-LONG-RANDOM-REVERSE-TOKEN"
accept_udp = false
ports = ["8080=127.0.0.1:8081"]

[server.short]
listen = "0.0.0.0:443"
server_name = "iran.example.com"
acme_domain = "iran.example.com"
acme_email = "operator@example.com"
# For static TLS, omit the two ACME keys and set certificate/key PEM paths.
```

```toml
# Kharej
[client]
remote_addr = "127.0.0.1:3080"
transport = "tcp"
token = "REPLACE-WITH-THE-SAME-REVERSE-TOKEN"
connection_pool = 8

[client.short]
server = "iran.example.com:443"
server_name = "iran.example.com"
# ca_file = "/etc/backpack/tls/iran-public-ca.pem" # private trust only
```

Only the exact paired loopback target is allowed. Managed Short HTTPS supports
TCP forwarding and directional EOF without an external helper, and can join
[managed adaptive paths](managed-paths.md). It cannot carry native UDP, TUN,
another outbound proxy or MPTCP. Connection Test includes a managed Short HTTPS
case with temporary verified certificates. Start with a modest pool: control,
idle pool, active forwards and probes all consume the bounded logical sessions.
The fixed pool is limited to 32 and aggressive pool expansion is rejected to
reserve session capacity for control, probes and application connections.

## Trying it without changing an existing tunnel

On the **backend server**, choose a free public port and a local TCP service:

```sh
backpack short-https init --ip SERVER_PUBLIC_IP --target 127.0.0.1:BACKEND_PORT --port 8443 --dir /root/short-https-test
backpack short-https serve --config /root/short-https-test/server.json
```

`init` creates a new directory with owner-only files and refuses to overwrite
existing state. Copy **only `client.json`** to the connecting server using SSH.
It contains the public certificate and shared secret; keep it private. The
server key and `server.json` stay on the backend server.

Change `listen` in the copied `client.json` from `127.0.0.1:0` to a free local
port, for example `127.0.0.1:2096`, then run on the **connecting server**:

```sh
backpack short-https client --config /root/client.json
```

Connect to the displayed local TCP address to reach the fixed backend. Set an
explicit public listen address only if you intend to expose that backend.
Both processes stay in the foreground; Ctrl+C stops them. Existing BackPack,
Backhaul, Xray and TUN listeners are not stopped or reconfigured.

## Limits and interpretation

This is connection recovery, not browser impersonation or guaranteed filtering
evasion. Native TLS 1.3 uses classical ECDHE to keep handshakes small; HTTP/1.1
uses a new connection per request. No custom cryptography or disabled certificate
verification is used. Repeated small requests and the native TLS fingerprint
remain observable. A filter that blocks the IP or the initial handshake can
still prevent it from working.

Each request carries at most 2 KiB in either direction. Throughput is lower and
TLS/CPU overhead higher than ordinary persistent tunnels. There are at most
64 logical streams and 64 KiB of buffered backend data per stream. Idle server
sessions expire after 90 seconds; a client gives up after 15 seconds without a
successful exchange. It cannot restore a backend TCP connection after process
restart or session expiry. The standalone relay supports TCP only. Managed
services, renewal and adaptive path selection use the integration above.

Standalone `init` certificates expire after seven days. Create a new paired test state
and transfer its new `client.json` when needed. Do not use this experimental
command as an unattended permanent replacement for a working tunnel.

## خلاصهٔ فارسی

<div dir="rtl">

این رلهٔ آزمایشی بدون دامنه و با IP کار می‌کند. به‌جای اتصال HTTPS طولانی،
درخواست‌های کوتاه با اتصال تازه می‌فرستد و شمارهٔ بایت‌ها را برای جلوگیری از
تکرار داده نگه می‌دارد. روی سرور مقصد، یک سرویس TCP محلی و پورت عمومی آزاد
انتخاب کنید؛ فقط فایل `client.json` را به سرور دیگر منتقل کنید و پورت محلی
آن را تنظیم کنید. گواهی خصوصی بررسی می‌شود و کلید خصوصی روی سرور مقصد می‌ماند.

حالت مدیریت‌شده در منوی HTTPS و Connection Test قرار دارد و با سرویس تانل،
لینک جفت‌سازی و گواهی ثابت یا ACME کار می‌کند. سرعت آن کمتر است و UDP را حمل
نمی‌کند. گواهی هفت‌روزه مخصوص دستور آزمایشی مستقل است. عبور از فیلترینگ تضمین
نمی‌شود و ظرفیت مسیر واقعی باید اندازه‌گیری شود.

</div>
