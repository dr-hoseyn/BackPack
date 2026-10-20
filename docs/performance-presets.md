# Performance presets

Presets expand into concrete settings when creating a tunnel or choosing
**Edit → Change performance preset**. Apply the same profile on both ends.
Updating the binary preserves existing configuration values and manual tuning.

| Profile | Reverse pool / streams per mux session | Stream / session receive window | Use |
|---|---:|---:|---|
| Balance | 4 / 32 | 256 KiB / 4 MiB | Small VPS instances |
| Turbo | 16 / 256 | 8 MiB / 16 MiB | General use |
| Aggressive | 32 / 512 | 16 MiB / 32 MiB | Bursty interactive traffic |
| Throughput | 64 / 512 | 32 MiB / 32 MiB | Sustained bulk traffic; needs RAM |

Throughput is available for all supported reverse transports: TCP, Stealth,
TCP Mux, WS/WSS, WS/WSS Mux, KCP, XDI, PCK, UDP and QUIC. Managed HTTPS modes
use the TCP reverse profile. Short HTTPS keeps its fixed spare pool at no more
than 32 because its relay protocol has a finite session table.

The adaptive pool samples demand every second and grows in batches. It can
expand up to 32 times its configured floor. Multiplexed sessions no longer stop
growing at a fixed 128 MiB aggregate receive-window budget. Spare connections
and physical sessions are counted separately: opening idle TCP connections or
QUIC streams does not divide the bandwidth of an existing transfer. Physical
mux sessions stay warm until their connection or generation ends. They never
discard future capacity requests as a substitute for retiring a busy session. Repair and demand-driven
dials share one ceiling, including connections still being established.

## Sizing for 400–500 Mbit/s

500 Mbit/s at 200 ms RTT needs **12.5 MB in flight**. The bulk profile's 32 MiB
stream window exceeds this bandwidth-delay product. Its KCP-family send and
receive windows remain 4,096 segments per physical session, with batched ACKs,
a 20 ms tick and 10:1 FEC. Four parallel sessions cover that aggregate target.
A single KCP session at 200 ms RTT still has a window ceiling below 200 Mbit/s.
The 16,384-segment candidate reduced measured concurrent goodput through ACK
processing and bursts; capacity comes from parallel sessions and CPU workers
instead. Gaming profiles retain their smaller KCP windows and immediate ACKs.

QUIC stream windows grow from 1 MiB to 32 MiB, and its connection window from
4 MiB to 64 MiB. TCP buffers remain kernel auto-tuned unless `so_pin_tcp` was
explicitly enabled. Assignment workers use all CPUs available to Go rather
than stopping at four. Configs omitting pool or mux settings now default to
32 warm connections, 256 streams/session, a 16 MiB stream window and a 32 MiB
session window. Missing datagram socket buffers default to 16 MiB.

Direct L4's Throughput profile uses at least four sessions, 32 MiB per stream
and 32 MiB per session. Its Turbo stream window is 8 MiB. Direct configs without
explicit windows default to 16 MiB per stream and 32 MiB per session. The L3
Throughput profile uses 32 MiB socket buffers with a 4,096-packet queue and
`fq_codel`; it does not add retransmission beneath IP or change the path MTU.

Receive windows are lazy ceilings, but their memory cost under load is real:
64 fully occupied 32 MiB receive windows can retain **2 GiB**, and automatic
growth can use more. Several high-capacity tunnels share the same host resources.
The profile leaves enough software capacity for the target; actual throughput
depends on the filtered, lossy or shaped path and carrier. Configured
`max_connections` and `bandwidth_mbps` remain in effect.

## Verification

The capacity regression keeps old forwards open and makes new ones force more
sessions over the real control channel. The opt-in measurement runs actual
preset-rendered tunnels with one and eight concurrent 16 MiB transfers:

```sh
go test -race ./internal/e2e -run TestMuxCapacityGrowsWhileExistingForwardsStayOpen
BP_CAPACITY_BENCHMARK=1 go test ./internal/e2e -run TestBulkProfileTransportCapacity -v -count=1
```

The Throughput capacity workflow measures the same transfers on the candidate
and base revision on one disposable Linux runner. Reports label application
goodput in one direction; echo traffic also crosses back, so full-duplex wire
traffic is about twice that number. Loopback measurements isolate software
overhead. Confirm sustained performance on the actual two-server path before
calling a deployment a verified 500 Mbit/s link.

<div dir="rtl">

## خلاصهٔ فارسی

پروفایل **Throughput** برای همهٔ ترنسپورت‌های Reverse و همچنین Direct و L3
در دسترس است. ظرفیت نشست‌ها و پنجره‌های جریان برای بار سنگین بیشتر شده و
رشد ظرفیت هر ثانیه و به‌صورت دسته‌ای انجام می‌شود. سقف ثابت ۱۲۸ مگابایتی
که جلوی رشد نشست‌ها را می‌گرفت حذف شده است. این تنظیم می‌تواند RAM و CPU
بیشتری مصرف کند؛ سرعت واقعی به مسیر، تأخیر، افت بسته و محدودیت کریر هم بستگی دارد.

برای تونل موجود، روی هر دو سرور از **Edit → Change performance preset**
پروفایل Throughput را اعمال کنید. نصب نسخهٔ جدید به‌تنهایی مقادیر دستیِ
کانفیگ موجود را تغییر نمی‌دهد. نتیجهٔ بنچمارک محلی تضمین سرعت ۵۰۰ مگابیت
بین دو سرور نیست و باید با تست پایدار روی همان مسیر تأیید شود.

</div>

[← Back to the docs index](README.md)

*Last verified against Backpack v1.14.1 source with the throughput-capacity changes.*
