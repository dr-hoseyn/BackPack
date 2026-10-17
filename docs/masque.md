# MASQUE over HTTP/3 (personal fork)

This fork adds `carrier = "masque"` to the layer-3 engine, using real
[RFC 9298 CONNECT-UDP](https://www.rfc-editor.org/rfc/rfc9298.html) and HTTP/3
Datagrams. It carries the existing Noise/GRE tunnel, including routed IP and
optional forwarded TCP/UDP ports. It uses pinned `masque-go v0.6.0` in the
Backpack process, with no helper binary.

This is the HTTP/3, UDP-based mode. HTTP/2 and Xray CONNECT-IP interoperability
are **not implemented** by this carrier. UDP/QUIC must work on the route.
HTTPS framing does not guarantee that a network will allow it or make it faster.

## Set up a pair

Install this fork's build on both servers. Allow UDP/443 to Kharej. Kharej needs
a PEM TLS certificate and private key; its certificate must cover the hostname
Iran verifies. Iran uses normal system trust, or an explicit private CA file.
TLS verification cannot be disabled. Certificate renewal is the operator's
responsibility; restart the listening tunnel after replacing certificate files.

Kharej:

```toml
[l3]
mode = "listen"
addr = "0.0.0.0:443"
carrier = "masque"
token = "REPLACE_WITH_A_LONG_RANDOM_SHARED_SECRET"
masque_target = "127.0.0.1:62101"
masque_cert = "/etc/letsencrypt/live/kharej.example.com/fullchain.pem"
masque_key = "/etc/letsencrypt/live/kharej.example.com/privkey.pem"
masque_server_name = "kharej.example.com"
local_ip = "10.20.0.2/30"
peer_ip = "10.20.0.1"
iface = "bp0"
mtu = 1200
```

Iran:

```toml
[l3]
mode = "dial"
addr = "kharej.example.com:443"
carrier = "masque"
token = "REPLACE_WITH_THE_SAME_SHARED_SECRET"
masque_target = "127.0.0.1:62101"
masque_server_name = "kharej.example.com"
# For a private CA only; this file must exist on Iran:
# masque_ca = "/etc/backpack/tls/ca.pem"
local_ip = "10.20.0.1/30"
peer_ip = "10.20.0.2"
iface = "bp0"
mtu = 1200
ports = ["8443=127.0.0.1:2096"]
# Optional, forwards UDP on these mappings as well:
# accept_udp = true
```

Each config runs with `backpack -c /path/to/config.toml` as root on Linux
because the L3 engine creates a TUN interface. Each server can use its own
interface name. Auto-MTU remains enabled by default; the conservative initial
MTU above accommodates QUIC, HTTP Datagram and Noise framing.

## Menu, panel and setup links

Choose **Direct → MASQUE (HTTP/3)** in the CLI or panel. The CLI asks for
certificate paths when building Kharej and an optional private CA file on Iran.
The panel's Iran form includes the TLS hostname, target and optional CA path.

The setup link carries the shared target and TLS hostname. It does not carry
certificate, key or CA files, or their local paths. On Kharej, paste the link
into **Setup from a link** to provide its local certificate files, or run:

```sh
sudo backpack link apply 'backpack://YOUR_LINK' \
  --masque-cert /path/fullchain.pem --masque-key /path/privkey.pem
```

When creating Iran from a Kharej link with a private CA, provide the CA path
in the menu. Transfer that public CA certificate separately. Never transfer
the TLS private key to Iran. Protect configs and setup links: both contain the
tunnel secret.

## Connection Test

MASQUE is included among the direct Connection Tests on IPv4 and IPv6. The test
creates a temporary certificate on Kharej and shares only the public trust anchor
with Iran. No domain or existing certificate is required for this test. It uses
a temporary UDP port and verifies real Noise/IP payload transfer and throughput;
a passing test does not prove that a permanent UDP/443 route will behave alike.

## Limits and troubleshooting

- `masque_target` is a numeric loopback IP and UDP port on Kharej, defaulting to
  `127.0.0.1:62101`. It is the only permitted CONNECT-UDP destination. Use a
  distinct private target port for each tunnel, identical in its paired configs.
- The listener bounds both QUIC connections and authenticated CONNECT requests
  to 16, and request headers to 8 KiB. Connections use keepalives and an idle
  timeout. Connections that do not authenticate within 12 seconds are closed
  even if they keep sending QUIC keepalives. Closing the carrier releases the
  listener, backend and proxy flows.
- MASQUE authenticates HTTP requests using a credential derived from the tunnel
  token, over verified TLS. Noise still authenticates and encrypts tunnel data.
- FEC is available. Multipath is limited to plain UDP and is rejected here.
- A UDP/443 bind error means another service already holds that address. A
  private target bind error means another tunnel or local service holds its port.
- Certificate errors require correcting the hostname, trust bundle or certificate
  chain. A QUIC handshake timeout requires checking UDP reachability and the route.
- A successful handshake is insufficient: verify payload transfer, reconnection
  and sustained load on your own route before using this carrier in production.

<div dir="rtl">

## خلاصهٔ فارسی

این قابلیت مخصوص همین فورک است: MASQUE واقعی روی HTTP/3 برای تانل L3، همراه با
Noise و امکان فوروارد TCP و UDP. حالت HTTP/2 و اتصال مستقیم به MASQUE در Xray
در این پیاده‌سازی وجود ندارد. گواهی TLS معتبر یا CA خصوصی لازم است؛ پورت UDP
باید روی مسیر باز باشد. برای هر تانل، پورت خصوصی `masque_target` جدا انتخاب کن
و مقدارش را در هر دو سر یکسان بنویس. نتیجهٔ اتصال روی یک مسیر را به شبکه‌های
دیگر تعمیم نده.

</div>

---

*Last verified against Backpack v1.9.3.*
