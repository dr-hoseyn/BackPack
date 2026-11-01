# DNS and symmetric ICMP carriers

These direct L3 carriers address two differences identified in
[Golden-GRE's route measurements](https://github.com/LivingG0D/Golden-GRE/blob/22e3bd4458a03b7072e227cc19c4084c57c76b27/docs/FILTER.md):
a path can require a complete DNS query, or admit only one ICMP echo type.
Their implementation uses Backpack's existing encrypted GRE/Noise L3 engine,
including authenticated handshakes, per-session keys and replay protection.
It does not invoke Golden-GRE's relay or share its wire format.

Choose **Setup Iran → Direct → UDP → DNS (EDNS0)**, or **ICMP → ICMP Requests /
ICMP Replies**, then apply the setup link on Kharej. Both endpoints need the
matching build. All three carriers are included in actual Connection Test
traffic checks and Quick/Full download/upload measurements when healthy.
Connection Test fetches their paired settings in a separate authenticated
answer, keeping each settings response inside its single-packet budget.

## DNS

`carrier = "dns"` sends complete DNS queries directly to the peer. Each UDP
packet has one TXT question and an EDNS0 OPT record with a private-use option
containing the sealed L3 datagram. The question's labels are scoped to the
tunnel and change with the sequence. A generic DNS parser can recover the
complete message; encryption and anti-replay checks remain the L3 engine's job.

This is peer-to-peer UDP traffic, not transport through a recursive resolver.
It needs no public domain or authoritative DNS server. A real DNS resolver
will not carry it to the peer. The shape alone does not prove that any given
filter or provider accepts it; Connection Test must verify the actual route.

Both endpoints receive on the chosen fixed DNS port and send from an ephemeral
source port. Replies continue to target the peer's fixed receive port after a
source-port rotation. **Both servers must accept inbound UDP on that port.**
This differs from ordinary dial-only UDP. Choose a free port; 53 is the wizard
default, and an existing DNS service can already occupy it.

Iran example:

```toml
[l3]
mode = "dial"
addr = "192.0.2.2:53"
carrier = "dns"
token = "REPLACE_WITH_A_LONG_RANDOM_SHARED_TOKEN"
local_ip = "10.78.1.1/30"
peer_ip = "10.78.1.2"
mtu = 1280
dns_rotate = 6
dns_rate = 5000
paths = 1
```

Kharej uses `mode = "listen"`, `addr = "0.0.0.0:53"`, swaps the tunnel IPs,
and keeps the same token and port. `dns_bind` optionally chooses a literal
local receive/source IP; it is local and never copied to the other endpoint.
It must match the listener address's family. An explicit listener IP must
agree with `dns_bind`.

| Setting | Behavior |
|---|---|
| `dns_rotate = N` | Create a fresh ephemeral sending socket every N packets; 0 retains one source port |
| `dns_rate = N` | Cap outgoing DNS datagrams per second with a short burst; 0 disables the cap |
| `paths = N` | Listen on N consecutive ports starting at `addr`'s port, and spread outgoing packets across the peer's matching ports; 1–8 |
| `fec_data`, `fec_parity` | Existing paired Reed-Solomon FEC; every data/parity datagram is wrapped as DNS |

Rotation and rate are sending-side controls: they can differ by endpoint.
The setup link copies the initial values for convenience. Port ranges and FEC
must agree at both ends. The rate includes handshakes and parity packets;
setting it too low can delay startup or reduce throughput. Rotating source
ports consumes ephemeral ports and can increase reordering on some routes.
Start with rotation/rate off, then tune only when measurement supports it.

DNS adds 50 bytes to UDP's normal IP+UDP overhead, which the L3 MTU calculation
includes. Existing UDP batching/offload remains unchanged; the DNS path uses
bounded receive queues and reusable buffers. Closed sockets and cancellation
also wake paced writers and readers waiting on a deadline.

## Symmetric ICMP

| Carrier | Outgoing traffic at both endpoints | Appropriate route |
|---|---|---|
| `xdi` | Existing request/reply split | Normal ping request/reply behavior |
| `icmp-request` | Echo requests only | Unsolicited echo replies are dropped |
| `icmp-reply` | Echo replies only | Requests are dropped but unsolicited replies pass |

Both symmetric modes support IPv4 and IPv6 and need Linux raw-socket privileges.
The ICMP type is fixed by the carrier name, so the setup link and Connection
Test cannot silently disagree about it. Session tags, direction checks and
identifier filtering continue to separate tunnels and reject the kernel's
copies. Request-only mode suppresses only this tunnel's automatic echo copies
on both endpoints; it does not disable ordinary host ping or ping inside TUN.

## Existing features and differences

FEC, BBR/host tuning, systemd management and port mapping already existed in
Backpack. These carriers reuse those facilities. The FEC settings here remain
paired; Golden-GRE's automatic receiver-side inference is not this wire format.
Encryption is always present in Backpack's L3 path, and replay protection is
retained. Golden-GRE's reported throughput applies to its measured routes and
does not establish Backpack's throughput on another path.

The namespace integration tests exchange authenticated IP packets, full-sized
pings, verified TCP/UDP forwarding and FEC traffic over IPv4 and IPv6, then
check teardown. DNS additionally exercises per-packet source-port rotation,
two receiving ports and a packet-rate cap.

<div dir="rtl">

## خلاصهٔ فارسی

سه حامل جدید از منوی Direct در دسترس‌اند: DNS با پیام کامل و معتبر DNS،
ICMP Request در هر دو جهت، و ICMP Reply در هر دو جهت. رمزنگاری Noise و محافظت
ضدبازپخش L3 حفظ شده‌اند و هر سه روش وارد Connection Test می‌شوند.

DNS مستقیم بین دو سرور است و به resolver یا دامنه نیاز ندارد. هر دو سرور باید
پورت انتخابی را باز داشته باشند. چرخش پورت مبدأ و سقف بسته در ثانیه اختیاری‌اند؛
تنظیم مسیر را با اندازه‌گیری انجام بده. تعداد پورت‌های متوالی و FEC در دو سمت
باید یکسان باشند. تنظیم IP محلی به سمت مقابل منتقل نمی‌شود. روش‌های ICMP، پینگ
معمولی سرور را خاموش نمی‌کنند.

</div>

---
[← Back to the docs index](README.md)

*Last verified against Backpack v1.19.2*
