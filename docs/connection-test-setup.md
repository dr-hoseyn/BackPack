# Automatic setup after Connection Test

In the terminal, **Quick** is the default test depth. It observes
each tunnel for 30 seconds, with connection discovery bounded to 20 seconds
(30 seconds for direct tunnels). It then measures download and upload on up to
three lossless candidates, ordered by RTT plus twice jitter, with connection
time breaking ties. This shortlist finds promising candidates; it does not
compare the speed of every protocol. Each selected IP path runs separately.

**Full** observes traffic for 60 seconds and measures every healthy tunnel.
Both depths enforce the observation deadline even when replies stop arriving.
Speed tests remain sequential so they do not compete for bandwidth. Connection
setup and optional spoofing/MTU checks add time to the observation window.

Quick results are labelled as a short observation. **SCREENED** means the
traffic check passed but speed was not measured; these rows retain their
measurements and are excluded from recommendations and automatic setup. Run
Full to measure every healthy candidate. Quick and Full remain separate in
history comparisons. Older links and Go callers without a depth retain Full; the terminal explicitly defaults to Quick.

Run Connection Test on Iran and join it on kharej using the test link as usual.
Both servers must run a version that supports automatic setup, as root. Keep
both test processes open until setup completes.

After the verdict, choose **Set Up A Tested Tunnel** on either server. Select a
successful Reverse, Direct, or AmneziaWG tunnel and enter the forwarded ports.
For example, `443` exposes Iran's port 443 and reaches kharej's local port 443;
`443=127.0.0.1:2096` reaches kharej's local port 2096 instead. Optionally enable
UDP alongside TCP. The reverse UDP transport carries UDP on its own.

Only one server needs those answers. The other side's background setup agent
receives its settings and creates its service automatically, even while its
after-test menu is still waiting for input. No second setup link or SSH login
is needed. To let the other operator choose, select **Wait For Setup**.
A kharej command without interactive input waits for the other side automatically.

The setup session lasts 15 minutes after the test. Each measured IP path can create one
permanent pair. Only tunnels marked successful are eligible; unstable tunnels
and the IP spoofing probes are excluded. The permanent pair keeps the tested
transport, tunnel port, preset, and paired settings. Temporary echo mappings
are replaced with the requested mappings, and a new shared security token and
new tunnel names are generated automatically.

Both configurations are checked first. Iran starts its service, kharej starts
its service, and Iran checks that the tunnel connects before reporting success.
Repeated requests with the same ID read back the same operation. On a failure,
the agents attempt to remove their newly created pair and report the error;
existing tunnels are never selected for replacement. The temporary test
engines are stopped before the permanent services reuse their ports.

The test coordinator remains available during this window. It accepts setup
messages only with the test-link secret, over the same TCP/UDP port used by the
test. Treat the test link as a management credential while that session is
open. Closing either test early cancels incomplete setup; the kharej aborts if
the coordinator stops answering. A process or machine crash can interrupt
cleanup, so inspect the reported tunnel names before retrying after a crash.
Completed services persist across reboots and no longer depend on the test
coordinator.

## Complete protocol coverage and prerequisites

The setup selector includes every successful measured tunnel: native Reverse,
Short HTTPS, Naive, XHTTP/REALITY, Hysteria2, AnyTLS, ShadowTLS, TUIC,
TrustTunnel, WebTransport, Tailcat, all Direct carriers, and AmneziaWG.
The two IP-spoofing capability probes are measurements, not installable tunnels.

Private helper keys stay on their original endpoint. Each side retains its own
verified test configuration privately, copies required certificates/keys/CA files
to `/etc/backpack/setup/<new-tunnel-name>/` with private permissions, and rotates
paired authentication. Permanent services do not reference the deleted test
workspace. Updating both endpoints is required for this coordinator capability.
Retained test certificates use the same ten-year validity as normal generated
Backpack certificates, so permanent tunnels do not expire an hour after testing.

AmneziaWG requires root and TUN access. Its backend must listen on the peer TUN
address or a wildcard. Automatic setup maps `127.0.0.1`/`localhost` destinations
to that peer TUN address and refuses destinations outside the peer host route.
The Spoof carrier uses IPv4 and is explicitly skipped on an IPv6-only path.
Missing optional helper binaries are also reported as skipped.

Tailcat additionally needs its separately pinned optional helper and an explicit
DERP choice in `/etc/backpack/conntest-tailcat.json`, for example:

```json
{"derp_map_url":"https://your-selected-provider.example/derpmap","derp_region":1}
```

Use the region actually present in that operator-selected map; no public relay
is silently selected. This configuration is shared through the authenticated
test coordinator so that the peer uses the same rendezvous selection.

<div dir="rtl">

## خلاصهٔ فارسی

پس از Connection Test، مسیر IP و تونل موفق را انتخاب کنید و پورت‌های فوروارد
را فقط در یک سمت وارد کنید. عامل سمت مقابل تنظیمات را خودکار دریافت می‌کند؛
هر دو سرور باید نسخهٔ جدید را با root اجرا کنند و تست تا پایان راه‌اندازی باز
بماند. نشست راه‌اندازی تا ۱۵ دقیقه پس از پایان تست فعال است.

تونل‌های بومی، حامل‌های دارای helper و AmneziaWG پشتیبانی می‌شوند. فایل‌های
خصوصی هر سمت روی همان سرور نگه داشته می‌شوند و گواهی‌ها در پوشهٔ دائمی خصوصی
کپی می‌شوند. در صورت شکست، پیکربندی تازه جمع می‌شود. ابزار اختیاریِ نصب‌نشده
یا Tailcat بدون نقشه و منطقهٔ DERP انتخاب‌شده، skipped گزارش می‌شود؛ نتیجهٔ
موفق ساختگی ثبت نمی‌شود. سرویس پشت AmneziaWG باید روی IP تونل یا wildcard
گوش کند. این حامل به root و TUN نیاز دارد.

</div>

*Last verified against Backpack v1.16.0*
