# Failover & load balancing

A client tunnel can hold more than one address for the same server, so a single
filtered IP or blocked port does not take the tunnel down.

## Add backup addresses

On the **client** (kharej), go to **Edit → Backup server addresses** and enter a
comma-separated list:

```
1.2.3.4, 5.6.7.8:8443, edge.example.com:443
```

If the main address stops answering, the client fails over to the next one
automatically. This is what keeps a tunnel alive after a server IP gets
filtered — the most common way a route dies on this path.

## Automatic failover (health-scored)

Plain failover only reacts when an address stops answering. A route usually gets
*worse* long before it dies — latency climbs, jitter sets in, a few percent of
packets drop — and none of that trips a failed connection. **Automatic failover**
handles that: it scores every address every few seconds by

```
score = mean_rtt + 2 × jitter + 20 × loss%      (lower is better)
```

and keeps traffic on the healthiest exit. Jitter and loss are weighted far above
raw latency because a steady 90 ms exit beats a 60 ms one that stutters — a
stutter is a lost frame, latency alone is not. A new exit only takes over when it
is at least **15% better for three checks in a row**, so the choice does not flap.

Turn it on at setup when a tunnel has backup addresses, or later from the menu.
For an existing tunnel use **Edit → Automatic Address Failover**. A health-based
switch reconnects the control channel and connection pool so active mux sessions
also leave the degraded route. Existing connections are interrupted during the
reconnect; sessions are not migrated between IP addresses.
It is the multi-exit setup for gaming, and it **replaces** load balancing — one
picks the single best exit, the other spreads across all of them, so only one can
be on at a time.

**Manage → Exit Health** scores and ranks every address on demand, and offers to
pin the healthiest as the primary — the manual version of the same thing.

TCP-based control channels verify the tunnel port too. Direct routes use ping
quality when available, and TCP connection timing when ICMP is blocked or ping
is missing. Routes with a configured proxy or source/interface binding use TCP
connection timing through that same route. For TCP measurements, loss means
failed connection attempts rather than ICMP packet loss. Datagram-only carriers
(KCP/QUIC/raw carriers) and the manual **Exit Health** view still rely on ICMP;
use failure-only fallback if their endpoints block it. A successful TCP connect
does not prove the tunnel's authentication handshake or payload works. A fixed
WebSocket `edge_ip` still pins the physical destination; changing origin backup
addresses cannot switch that fixed CDN edge between IPv4 and IPv6.

## IPv4 primary with IPv6 backup

IPv4 and IPv6 addresses can be mixed in the same reverse tunnel. On the client,
keep IPv4 as the main address, add the **same server's** IPv6 under
**Edit → Backup Addresses**, then enable **Edit → Automatic Address Failover**:

```toml
[client]
remote_addr = "192.0.2.10:443"
fallback_addrs = ["[2001:db8::10]:443"]
health_failover = true
```

These are example addresses; replace them with your server's real addresses.
Keep the rest of your existing transport and token settings. IPv6 addresses
with a port must use brackets. Both addresses must reach the same tunnel
listener and port, and the chosen transport must support both IP families.
If the server is bound only to its IPv4 address, adjust its listener first;
adding a client backup alone cannot make the server listen on IPv6. Both hosts
also need working IPv6 routing and firewall access to the tunnel port.

The client starts with the main address. Failed connection attempts try the
backup; quality scoring switches when the other path is at least 15% better
for three consecutive checks, or immediately after a check finds the current
path unreachable and the backup healthy. The same rules can switch it back to
IPv4. This selects the healthiest path, so a healthy but consistently better
IPv6 route can also take over. It measures ping latency, jitter and loss, not
available bandwidth. Leave `health_failover` off for failure-only fallback.

## Load balancing

By default the extra addresses are only spares. Turn on **load balancing** and
they become active routes at the same time: the tunnel's data connections are
spread across all of them, so one throttled route slows only its own share of
the traffic rather than the whole tunnel. The control channel stays pinned to a
single address, since that is what identifies the peer.

## The one rule

**Every address must reach the same server.** That can be:

- a second IP of the same server,
- another of its ports, or
- a CDN edge in front of it.

Pointing them at different machines will not work — the token and the exposed
ports belong to one server.

## CDN edge (hiding the server's IP)

At client setup you can enter an **Edge IP** — the client then connects to a CDN
edge (e.g. Cloudflare) instead of the server's origin, and the CDN forwards to
the server. The origin IP is never exposed to the client side, which is one way
to keep a server's address off a blocklist. This works with **WSS/WSS Mux on a
CDN-proxied port** (443, 8443, 2053, …); a raw transport cannot go through a CDN.

See [When a server is filtered, blocked, or its IP is dirty](filtered-or-dirty-ip.md)
for the full picture.

---

<div dir="rtl">

## خلاصهٔ فارسی

یک تونل کلاینت می‌تواند چند آدرس برای **همان سرور** داشته باشد تا فیلتر شدن یک
آی‌پی یا بسته شدن یک پورت، تونل را از کار نیندازد.

**آدرس‌های پشتیبان:** روی سرور خارج، `Edit → Backup server addresses` و لیستی با
کاما مثل `1.2.3.4, 5.6.7.8:8443, edge.example.com:443`. اگر آدرس اصلی جواب ندهد،
کلاینت خودکار سراغ بعدی می‌رود.

**فِیل‌اوور خودکار (امتیازدهی سلامت):** فِیل‌اوور ساده فقط وقتی واکنش نشان می‌دهد
که آدرسی کاملاً بمیرد، ولی مسیر معمولاً خیلی قبل‌تر **بد** می‌شود. این حالت هر
چند ثانیه همهٔ آدرس‌ها را با فرمول `rtt + 2×jitter + 20×loss%` امتیاز می‌دهد و
ترافیک را روی سالم‌ترین خروجی نگه می‌دارد. جیتر و افت خیلی بیشتر از تأخیر خام
وزن دارند، چون یک خروجی ۹۰ms پایدار بهتر از ۶۰ms پرنوسان است. یک آدرس جدید فقط
وقتی جایگزین می‌شود که **حداقل ۱۵٪ بهتر باشد، سه بار پشت سر هم** — تا انتخاب
مدام تغییر نکند. این همان ست‌آپ چند-خروجی مخصوص بازی است و **جایگزین** لود
بالانس می‌شود.

**لود بالانس:** به‌جای اینکه آدرس‌های اضافی فقط یدکی باشند، همه هم‌زمان فعال
می‌شوند و اتصال‌ها بینشان پخش می‌شود، پس یک مسیر throttle‌شده فقط سهم خودش را کند
می‌کند.

**تنها قانون:** همهٔ آدرس‌ها باید به **همان سرور** برسند — آی‌پی دوم همان سرور،
پورت دیگرش، یا یک لبهٔ CDN جلویش. آدرس‌هایی که به ماشین دیگری می‌روند کار
نمی‌کنند.

**نکته:** مسیرهای دارای کانال کنترل TCP، پورت تانل را هم بررسی می‌کنند و اگر
پینگ بسته یا نصب نباشد، از زمان اتصال TCP استفاده می‌کنند. بررسی پراکسی و
تنظیمات خروجی از همان مسیر واقعی عبور می‌کند. برای حامل‌های بدون کانال TCP و
نمای دستی `Exit Health`، امتیازدهی همچنان به ICMP نیاز دارد. موفقیت اتصال TCP
به‌تنهایی صحت احراز هویت و انتقال دادهٔ تانل را ثابت نمی‌کند. تنظیم ثابت
`edge_ip` نیز مقصد CDN را ثابت نگه می‌دارد و با تغییر آدرس پشتیبان عوض نمی‌شود.

**فالبک IPv4 به IPv6:** روی کلاینت، IPv4 را آدرس اصلی نگه دار و IPv6 همان سرور
را به شکل `[2001:db8::10]:443` در `Edit → Backup Addresses` وارد کن. سپس
`Edit → Automatic Address Failover` را فعال کن. سرور باید روی هر دو خانوادهٔ IP
گوش بدهد و مسیر IPv6 و پورت تانل در دسترس باشد. هنگام تغییر مسیر، کانال کنترل و
پول اتصال‌ها دوباره ساخته می‌شوند؛ اتصال‌های جاری ممکن است قطع شوند و مهاجرت
بدون وقفهٔ سشن انجام نمی‌شود. بازگشت به IPv4 هم با همان معیار سلامت انجام می‌شود.
این حالت تأخیر، جیتر و افت بسته را می‌سنجد، نه پهنای باند؛ برای فالبک فقط هنگام
قطعی، `health_failover` را خاموش نگه دار و فقط آدرس پشتیبان را ثبت کن.

</div>

---
[← Back to the docs index](README.md)

---

*Last verified against Backpack v1.17.0*
