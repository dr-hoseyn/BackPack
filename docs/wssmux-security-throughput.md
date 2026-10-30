# WSS Mux authentication, framing and capacity

New WS Mux/WSS Mux peers negotiate the standard `binary` WebSocket subprotocol
and carry smux bytes in RFC 6455 binary messages. Frames retain client masking;
control frames and message boundaries are processed by Gorilla. The stream
adapter has no batching timer, uses the existing pooled 64 KiB writers, and
does not enable per-message compression for already encrypted proxy traffic.
The adapter exposes smux's scatter/gather interface: the mux header and payload
share one message without a private 64 KiB staging buffer per physical session.
The pooled writer fits a maximum smux payload plus its eight-byte header, so
maximum-size payloads do not create a second tiny WebSocket fragment.
Direct WS/WSS peers use the same negotiated framing.

An older peer that does not acknowledge `binary` retains its raw wire format.
This preserves rolling upgrades; it does **not** make a legacy raw connection
compatible with a proxy that parses WebSocket messages. Upgrade both peers
before relying on message-aware proxy/CDN interoperability. No new subprotocol
is sent on the reverse control channel, which already uses real WebSockets.

## Server authentication and upgrade order

WSS endpoints, including a WSS fallback, reject an empty token or the public
built-in compatibility token. A known default lets a probe compute both exporter
proofs and claim the tunnel; TLS cannot turn a public token into authentication.
Provision the same strong, independently generated private token on both peers.
The existing plain-TCP compatibility policy is unchanged.

Reverse WSS/WSS Mux now require a valid session-bound server proof on the very
first connection. Upgrade the server before the client. Self-signed direct
origins remain usable because both sides prove the high-entropy tunnel token
against the TLS exporter. A missing proof is no longer silently treated as an
old server, and restarting the client cannot erase an authentication requirement.

`simple_auth = true` is for a trusted TLS terminator. The client now verifies
the certificate and hostname **before** sending the bearer token. Public CA
certificates need no additional configuration. For a private CA/self-signed
origin, set `wss_ca_file` on the client to its trusted PEM CA/certificate. Roots
are loaded once per transport configuration; reload the client to apply changes.
`wss_server_name` selects the TLS name, SNI and HTTP Host when dialing an address literal, but
prefer a domain in `remote_addr` and an optional `edge_ip` override so Host and
SNI naturally name the same origin. CA paths are local to the client and must
be provisioned there; sharing a server setup link does not provision trust.

If a server cannot yet produce proofs, `wss_allow_legacy_server = true` is an
explicit temporary compatibility option. It also **requires verified TLS**,
even when `simple_auth` is false. A wrong proof is always rejected. There is no
option to send the bearer token to an unauthenticated TLS peer.

TLS termination breaks exporter binding across the proxy. Set `simple_auth`
on both sides of a trusted terminating proxy and verify its public certificate;
do not try to repair exporter binding by suppressing an authentication failure.
Use an actual web server/site when web responses are required; the built-in
decoy's nginx header does not emulate nginx's entire TLS/HTTP implementation.
Idle public HTTP keepalives are bounded at 75 seconds; upgraded WebSocket
connections are hijacked and are not expired by this HTTP timeout.
Static GET/HEAD, POST-to-missing and unsupported-method responses are checked
against nginx's static-handler behavior, including consistent 405 error pages.

## TLS and HTTP consistency

The reverse client pins uTLS's Chrome 133 preset and uses a matching Chrome UA,
instead of choosing inconsistent UA families and sending a custom user-id
header. This is a pinned implementation, not an automatically current browser.
Review the ClientHello and UA together when updating uTLS.

By default, direct peers still see the existing full HTTPS preset's ALPN offer.
An endpoint selecting h2 needs HTTP/1.1 fallback because this implementation does
not implement RFC 8441. That capability is cached for ten minutes, with at most
128 entries, keyed by origin, dial destination and TLS name; a handshake failure
invalidates it. Pool connections reuse the control connection's authenticated
decision. An unproven peer cannot populate the cache.

`wss_http11_only = true` offers HTTP/1.1 from the first handshake and removes
the unusable h2 probe. Chromium has an HTTP/1.1-only path for fresh WebSocket
sockets; claiming that a browser never produces that ALPN is incorrect.
See the [Chromium implementation](https://chromium.googlesource.com/chromium/src/+/1d8eb53fa47e49798dabd5d04460b1b8a0c163c7/net/http/http_stream_factory_job.cc).
This option is explicit so existing deployments retain their selected behavior.
It changes the ClientHello; neither ALPN choice proves resistance to blocking.
Both modes have TLS-handshake tests, and authentication is always independent.
The HTTP/1.1-only offer also removes the preset's h2-only ALPS extension.
Advertising application settings for an unoffered protocol conflicts with
[BoringSSL's ALPS contract](https://boringssl.googlesource.com/boringssl/+/HEAD/include/openssl/ssl.h).
A serialized-ClientHello regression test checks ALPN/ALPS consistency; the full
HTTPS preset retains its h2 settings.

These changes improve protocol consistency and authentication. They do not
guarantee resistance to traffic analysis or blocking. Timing, volume, endpoint
reputation and connection lifetime remain observable. HTTP headers and raw
application framing inside WSS are not visible to an ordinary passive observer
without TLS keys; a TLS terminator can inspect them.

## A bounded 600 Mbit/s starting profile

600 Mbit/s at 200 ms RTT has an aggregate bandwidth-delay product around 15 MB.
The following settings leave a 16 MiB per-stream window for that case, share
32 MiB of receive credit per physical session, and bound pool growth. They are
an opt-in starting point, not a WAN throughput promise or a new global preset.

Apply these on **both** sides:

```toml
transport = "wssmux"
nodelay = true
mux_version = 2
mux_framesize = 32768
mux_recievebuffer = 33554432
mux_streambuffer = 16777216
```

On the server, use `mux_con = 128`. On the client:

```toml
connection_pool = 8
wsmux_max_sessions = 16
aggressive_pool = false
so_pin_tcp = false
# Optional fresh-WebSocket ALPN; retains a Chrome 133 preset for other fields.
wss_http11_only = true
```

These are keys inside the existing `[server]` and `[client]` tables, not a
complete deployment configuration. Retain the existing token, addresses,
certificate settings and port mappings. In a client config the informational
`mux_session` key does not set server admission; `mux_con` belongs on the server.

With 16 physical sessions, nominal aggregate receive-window capacity is
512 MiB **per endpoint**, before TLS, relay, smux and kernel socket overhead.
Windows are allocated lazily; this is neither initial allocation nor an RSS
limit. At 128 streams/session, admission capacity is at most 2,048 streams;
raise the explicit ceiling/admission settings if measured concurrency needs it.
`wsmux_max_sessions = 0` retains existing demand-driven growth. Nonzero values
include pending dials and must be at least `connection_pool`. Existing edits
preserve these settings and the client trust settings.

Do not constrain idle users using a theoretical sum of maximum buffer sizes.
Measure resident memory and live workload. Conversely, a large lazy window
still costs real RAM if enough peers fill it. Existing safe idle retirement and
per-generation session accounting remain active. Pool growth does not move an
existing stream to a new TCP connection; TCP head-of-line blocking within each
physical session remains.

## Reproducible verification

The transitive `klauspost/compress` dependency is pinned to 1.18.7 to include
[GHSA-259r-337f-4rfw](https://github.com/klauspost/compress/security/advisories/GHSA-259r-337f-4rfw)'s fix.
The advisory concerns S2 dictionary encoding; Backpack currently reaches this
module through uTLS's zstd dependency, not a confirmed attacker-controlled S2
encoding path. The patch removes the vulnerable dependency version without
claiming that the advisory was exploitable through this tunnel.

The `WSS Mux security and capacity` workflow runs race-checked authentication,
framing, message-aware proxy and reconnection regressions, then the bounded
profile's byte-verified download measurement with a 600 Mbit/s requested floor.
A separate workload holds 1,000 open users, paces offered application load at
600 Mbit/s in 32 KiB writes with a shared 64 KiB burst, and transfers payloads concurrently
and records CPU-seconds/application-GiB, sampled peak RSS and new-user p99 latency
throughout the bulk phase. The existing resource-comparison workflow also
measures unbounded bulk traffic and namespace-isolated delay/loss.
The quiet-user retirement test verifies that excess sessions disappear without
closing the remaining user. Logs are retained as workflow artifacts.

On a disposable Linux host, not a loaded production host:

```sh
BP_CAPACITY_BENCHMARK=1 BP_WSSMUX_BOUNDED=1 BP_CAPACITY_MIN_MBPS=600 \
  go test ./internal/e2e -run '^TestBulkProfileDownloadCapacity/wssmux$' -count=1 -v
BP_RESOURCE_BENCHMARK=1 BP_WSSMUX_BOUNDED=1 BP_RESOURCE_TARGET_MBPS=600 \
  go test ./internal/e2e -run '^TestWSMuxResourceProfile$' -count=1 -v
```

Loopback establishes host capacity and correctness, not the throughput or
detectability of an Iran/international path. Compare revisions at equal offered
load, runtime settings and CPU allocation; lower CPU caused by lower delivered
throughput is not an optimization. For delay/loss tests, use the existing
`BP_RESOURCE_NETEM` fixture only in a disposable network namespace: it changes
the namespace's loopback MTU, offloads and qdiscs.

Last verified against Backpack v1.18.0.

<div dir="rtl">

**خلاصهٔ فارسی**

دو سمت جدید، داده را داخل فریم معتبر WebSocket می‌فرستند؛ سمت قدیمی فقط با
قالب قدیمی خودش سازگار می‌ماند. ابتدا سرور را ارتقا دهید: کلاینت جدید از اولین
اتصال اثبات هویت سرور می‌خواهد. در simple_auth، گواهی و نام سرور پیش از ارسال
توکن بررسی می‌شود و برای گواهی خصوصی باید wss_ca_file را در کلاینت تعیین کنید.
پروفایل نمونه برای هدف ۶۰۰ مگابیت، نسخه mux برابر ۲، پنجره هر جریان ۱۶ مگابایت،
هشت اتصال گرم و سقف شانزده اتصال دارد؛ ظرفیت مسیر واقعی و مصرف حافظه باید
اندازه‌گیری شوند. این تغییرات تضمین شناسایی‌نشدن نیستند. بارگذاری آزمایشی و تغییر
MTU یا صف شبکه را روی سرویس دارای کاربر اجرا نکنید.

</div>
