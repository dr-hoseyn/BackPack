# XHTTP: separate download and bounded connection tuning

These settings extend the existing managed Xray wrapper. They are optional;
existing configurations retain verified TLS, HTTP/2 and `stream-up`, with the
pinned Xray helper's connection defaults. No raw JSON overrides or insecure TLS
switch is exposed. `packet-up`, `auto` and H3 remain unavailable in this wrapper.

On Kharej, **Edit → HTTPS Settings** offers optional advanced questions after
the normal XHTTP identity and certificate questions. The same settings can be
added to the existing `[client.xray]` table:

```toml
# Keep the existing binary, mode, server, uuid, server_name and path settings.
[client.xray]
# ... existing fields ...
max_connections = 2
max_requests = 800
max_reuse_seconds = 1800

download_server = "download.example.com:443"
download_server_name = "download.example.com"
download_host = "tunnel.example.com"
# download_path = "/the-existing-private-path"
# download_ca_file = "/etc/backpack/tls/download-ca.pem"
```

Do not add a duplicate TOML table; merge these fields into the existing one.
`download_server` selects a separate download connection. Both endpoints must
reach **the same XHTTP server/session**, possibly through different reverse
proxies or CDN edges. Two unrelated Xray servers do not share session state.
Changing the connection endpoint does not create another server listener or
automatically configure a DNS record, website, proxy or CDN.

`download_server_name` is mandatory when a download endpoint is set; its own
certificate is verified. `download_host` defaults to that name; `download_path`
defaults to the upload path. Use an alternate path only when the frontend maps
it to the same XHTTP handler. `download_ca_file` is separate trust for the download
endpoint; leaving it empty uses system roots, **not** the upload private CA.
Valid changes to either CA file reload the helper through the existing validated
reload mechanism; invalid or missing replacements keep the current generation.

## Connection controls

All values default to zero, meaning the pinned helper's existing default:

| Setting | Accepted explicit value | Effect |
|---|---|---|
| `max_concurrency` | 1–1024 | Maximum concurrent requests per connection |
| `max_connections` | 1–64 | Maximum connections; mutually exclusive with `max_concurrency` |
| `max_requests` | 1–100000 | Requests before connection renewal |
| `max_reuse_seconds` | 1–86400 | Connection reuse lifetime |

Overrides apply to upload and, when configured, download. The generator preserves
the pinned helper's other defaults when just one control is changed. These are
XHTTP's `xmux` controls, separate from BackPack's reverse connection pool and
smux. Larger values can consume more memory or create more long-lived sessions;
start modestly and compare verified throughput and recovery on the actual route.
Connection lifetime does not provide transparent migration of active streams.

These client-local choices survive normal edits, revisiting HTTPS Settings and
an XHTTP-to-XHTTP update from a peer's Setup Link. A new Setup Link builds the
normal default client and does not export local download/trust-file choices.
Changing to another protocol clears them. Both peer builds should be updated
when using managed carriers, as described in [Transports](transports.md).

## Reverse proxy / CDN deployment

Use a real domain and certificate at each terminating edge. The proxy must pass
streaming request bodies and stream responses without buffering, preserve the
paired private path (or deliberately map it), forward to the same origin and
support sufficiently long timeouts. Verify origin TLS separately. DNS and TLS
success alone do not prove XHTTP forwarding. Test upload and download, checksums,
idle periods, concurrent sessions and recovery after a helper restart.

The automated integration test runs the pinned helper through two independent
verified TLS/H2 reverse proxies and requires payload to traverse both. It does
not certify a particular commercial CDN or filtering environment.

<div dir="rtl">

## خلاصهٔ فارسی

تنظیمات پیشرفته در سمت خارج هستند. می‌توان برای دانلود آدرس، نام TLS، Host و
فایل اعتماد جدا تعریف کرد؛ هر دو مسیر باید به همان سرور و نشست XHTTP برسند.
اعتبارسنجی گواهی همیشه فعال است و CA آپلود خودکار به دانلود منتقل نمی‌شود.
مقادیر صفر، پیش‌فرض فعلی را نگه می‌دارند. تعداد اتصال و تعداد درخواست هم‌زمان
را با هم تنظیم نکن. سازگاری CDN و پایداری مسیر را با انتقال واقعی داده بسنج.

</div>

---

*Last verified against Backpack v1.18.1*
