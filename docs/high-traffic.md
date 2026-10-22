# Many concurrent connections to one backend

A reverse tunnel can stay connected while new user connections fail on the
exit server's final hop to Xray or another local backend. A log such as
`dial tcp ... ->127.0.0.1:443: connect: cannot assign requested address`
can mean that the source-port range is exhausted. The default 32768-60999
range has 28,232 ports per source-IP/backend-IP/backend-port tuple. Increasing
the tunnel's queue or restarting it does not increase this limit.

## Increase capacity without losing service ports

On Linux, explicitly enable the high-concurrency host profile:

```sh
sudo backpack tune high-concurrency --reserve-ports 62050,62051
```

This applies `10240 65535` (55,296 ports before reservations), preserving the
existing `ip_local_reserved_ports`, configured tunnel ports, and all current
IPv4/IPv6 TCP LISTEN ports, including loopback backends and unrelated panels.
Reservations are applied before the range is widened. Add fixed UDP service
ports and ports of services currently stopped to `--reserve-ports`; ranges
such as `20000-20010` are accepted. Transient outbound sockets are not reserved.
Reserve new fixed service ports before installing the service, or rerun the
command after changing the service inventory.

The command changes only these two host sysctls. It does not restart tunnels,
change auto-refresh/cron, grow buffers or queues, or promise a throughput gain.
Bandwidth, CPU, memory, file descriptors and backend limits still apply. An
installation forwarding more connections than one source IP can support will
eventually need multiple local backend addresses, ports or source addresses.

The profile is saved in `/etc/sysctl.d/zzzz-backpack-capacity.conf`, with its
original port policy in `/etc/backpack/high-concurrency-backup.json`. Rerunning
Optimize, applying a preset or updating preserves the selected capacity profile
and refreshes its reservations. Ordinary installations keep their default
range. Failed applications attempt to restore the live settings and report
rollback failures instead of claiming success. Host policy changes and Optimize
share a process lock; a conflicting operation reports that another update is in
progress before changing kernel settings or the rollback snapshot.

Restore the original range and reservations with:

```sh
sudo backpack tune restore
```

This leaves running tunnels and their restart schedule alone. It restores the
snapshot from before the first successful profile application; review any
manually added reservations before restoring. The separate MSS setting below
is a per-tunnel configuration and is not affected by host-profile restore.

## Size TCP packets for the measured path

Do not set every host to MTU 1492. Verify the path in both directions first.
For example, IPv4 `ping -M do -s 1464 PEER` sends a 1492-byte IP packet, whereas
`-s 1472` sends 1500 bytes. Repeat probes and compare with real TCP behaviour:
ICMP filtering can also make a probe fail.

If the measured IPv4 limit is 1492, use the existing per-tunnel TCP MSS clamp
through the command line on each server (with that server's tunnel name):

```sh
sudo backpack tune path-mtu server-8443 1492
sudo backpack tune path-mtu client-8443 1492
```

This stores an advertised MSS of 1452 (1492 minus the fixed 40-byte IPv4/TCP
headers). Linux deducts TCP options such as timestamps from the data payload
itself, following [RFC 6691](https://www.rfc-editor.org/rfc/rfc6691.html#section-2).
The command uses the existing validated tunnel-edit path. Each command **restarts its named
tunnel**; the configured auto-refresh schedule is unchanged. Existing
connections must be re-established to negotiate the new MSS. No default route,
host route or interface MTU is modified. This is for reverse TCP-based
transports, including WSS Mux; use the transport's packet-size settings for
datagram carriers. The IPv4 calculation is not an IPv6 MTU conversion.

Clear the clamp with `sudo backpack tune path-mtu NAME 0`, or edit TCP MSS in
the menu/panel. A preset change does not remove the clamp.

## Verify under the actual workload

Check the two sysctls, current backend connections and recent tunnel logs:

```sh
sysctl net.ipv4.ip_local_port_range net.ipv4.ip_local_reserved_ports
ss -Hnt dst 127.0.0.1:443 | wc -l
journalctl -u backpack-client-8443 --since '10 minutes ago'
```

Look for `cannot assign requested address`, queue-full drops and control-channel
errors. A short clean observation is not evidence of long-term stability.
Hourly restarts still disconnect existing users if the operator keeps them.

Raw UDP overload warnings share one reporting budget across this tunnel's
listeners. The first drop is reported immediately; while drops continue, later
reports appear at most once every five seconds and include the suppressed drop
count and the latest cause. A quiet journal therefore does not establish that
no packets were dropped. The payload queues remain bounded and drop new packets
when full, preserving those already queued. Raising a queue limit cannot create
bandwidth or repair a backend that is no longer draining traffic.

The optional regression test exhausts a small source-port range, enables the
profile and connects to the same backend again in an isolated network namespace:

```sh
sudo env "PATH=$PATH" BP_CAPACITY_NETNS_TEST=1 go test ./internal/optimize \
  -run '^TestHighConcurrencyRealLoopbackCapacity$' -count=1 -v
```

---

<div dir="rtl">

## خلاصهٔ فارسی

اگر تونل متصل است ولی اتصال به سرویس محلی با خطای
`cannot assign requested address` شکست می‌خورد، ممکن است پورت‌های موقت تمام
شده باشند. محدودهٔ پیش‌فرض حدود ۲۸ هزار پورت برای هر ترکیب مبدأ و مقصد دارد.
دستور `backpack tune high-concurrency --reserve-ports 62050,62051` ظرفیت را به
حدود ۵۵ هزار می‌رساند و رزروهای قبلی، پورت تونل‌ها و پورت سرویس‌های TCP فعال
را حفظ می‌کند. پورت سرویس‌های خاموش و سرویس‌های ثابت UDP را هم صریحاً اضافه کن.
این دستور تونل را ری‌استارت نمی‌کند و زمان‌بندی ری‌استارت ساعتی را تغییر نمی‌دهد.

تنظیمات بعد از بوت و اجرای دوبارهٔ Optimize حفظ می‌شوند. با دستور
`backpack tune restore` سیاست پورت‌ها به وضعیت پیش از اولین فعال‌سازی برمی‌گردد؛
اگر بعداً دستی رزرو جدیدی اضافه کرده‌ای، پیش از بازگردانی آن را بررسی کن.

عدد MTU را از روی تست مسیر در هر دو جهت انتخاب کن. اگر محدودیت واقعی IPv4
برابر ۱۴۹۲ است، دستور `backpack tune path-mtu NAME 1492` مقدار MSS تونل را روی
۱۴۵۲ ذخیره می‌کند؛ کرنل سربار گزینه‌های TCP را خودش محاسبه می‌کند.
این دستور را با نام تونل همان سرور در هر دو طرف اجرا کن؛
هر بار تونل انتخاب‌شده ری‌استارت می‌شود. مسیر و MTU اینترفیس سیستم تغییر نمی‌کند.
مقدار `0` محدودیت MSS را پاک می‌کند. برای ترنسپورت‌های دیتاگرامی باید اندازهٔ
بستهٔ خود ترنسپورت را تنظیم کرد؛ این تبدیل مخصوص IPv4 و TCP است.

افزایش پورت به معنی تضمین سرعت یا پایداری طولانی‌مدت نیست؛ مصرف CPU، رم و
محدودیت سرویس مقصد را هم زیر بار واقعی بررسی کن. اگر ری‌استارت ساعتی فعال بماند،
همچنان اتصال‌های موجود کاربران را قطع می‌کند.

</div>

*Last verified against Backpack v1.14.1.*
