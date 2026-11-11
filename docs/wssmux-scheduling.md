# WSSMux carrier scheduling

WS Mux and WSS Mux now assign a waiting user to an available carrier using
socket congestion first, active relay count second, and least-recent use to
break ties. Previously every session competed for the same wake-up channel,
so a quiet-looking session with a backed-up TCP socket could take the next
user even when another physical session was draining normally.

On Linux the scheduler uses the existing TCP_INFO observer: queued bytes,
delivery rate, receiver window and retransmission state estimate the delay in
front of a new stream. Samples are cached for 50 ms per physical session;
there is no additional accounting on application reads or writes. TLS and the
deadline wrapper are unwrapped only to identify the observed TCP socket. All
traffic still passes through the original TLS/WebSocket connection.
Idle sessions are not polled. TCP Forward retains its existing 200 ms cache.

Other platforms, unavailable kernel counters and unsupported wrappers fall
back to occupancy-based selection. No carrier is discarded just for having a
high score. A recovered carrier becomes eligible again at the next sample.
Closed sessions are excluded directly through their close notification, even
if socket sampling returns zero. If a selected session closes after waking its
worker, the user remains queued for a live session in the original FIFO order.
When all carriers have pressure, new users use the least congested available
one. FIFO order, pairing timeouts, queue capacity, stream admission limits,
pool growth and safe retirement retain their existing behavior.

Every available WebSocket carrier participates. This policy deliberately does
not use TCP Forward's separate warm-pair preference or its 2 ms grace period.
TCP Forward, TCP Mux and KCP keep their previous scheduling policy.

This is a server-side scheduling change with no new wire format or setting.
It helps place **new** streams; an existing stream remains on its original
carrier. It cannot remove TCP head-of-line blocking within a carrier.

## Related approaches

- [smux](https://github.com/xtaci/smux) supplies fair traffic shaping and
  version-2 per-stream flow control *inside* one session. Carrier selection
  handles the separate decision of which physical session takes a new user.
- [Envoy least request](https://www.envoyproxy.io/docs/envoy/v1.37.7/intro/arch_overview/upstream/load_balancing/load_balancers)
  uses active requests when selecting an upstream. Our generic streams do not
  expose HTTP request timing, so the congestion signal comes from the carrier
  socket rather than inventing application RTT measurements.
- [frp](https://gofrp.org/en/docs/features/common/network/network/) supports
  connection reuse and QUIC. [wstunnel](https://github.com/erebe/wstunnel)
  also provides WebTransport for paths where UDP works. Backpack already has
  QUIC/WebTransport carriers; changing transport is an independent choice.

## Verification

`TestWSMuxQueueSpreadsNewUsersAcrossEveryAvailableCarrier` checks that spares
participate immediately. Additional regressions check a stalled carrier with
fewer streams than a healthy carrier, recovery without a restart, and FIFO
reassignment when the selected carrier leaves. Existing mux queue, framing,
deadline and retirement tests continue to run.

`BenchmarkWSMuxCarrierSelection` measures only scheduling, not network
throughput. Compare complete transfers and fresh-user p95/p99 latency under
the same offered load before drawing conclusions about a WAN route.

An owner-hosted Linux loopback comparison is preserved in the
[raw measurements](benchmarks/wssmux-scheduling-2026-11-12.json). Three runs per
revision used eight bulk workers, 32 verified silent streams and a six-second
pause on one test-only carrier. Bulk and corruption errors were zero on both
revisions; fresh-probe failures were four before and one after. Median measured
application throughput was 19.891 and 19.876 Mbit/s at a 20 Mbit/s offered load.
Successful-probe p99 medians were 128.6 and 155.7 ms respectively. These small
samples demonstrate fewer failures in this scenario, not a general tail-latency
or WAN-throughput improvement; failed probes are reported separately from latency.
Those samples predate the follow-up closed-session admission fix. Its
regressions cover closure before selection and after notification, including
avoiding a warm-carrier wait when the warm sessions have already closed.

## خلاصهٔ فارسی

<div dir="rtl">

در WS Mux و WSS Mux، اتصال تازه ابتدا به نشستی سپرده می‌شود که گرفتگی TCP
کمتری دارد؛ تعداد استریم‌های فعال و ترتیب استفاده، انتخاب‌های برابر را مشخص
می‌کنند. اطلاعات گرفتگی فقط هنگام نیاز به پذیرش اتصال، حداکثر هر ۵۰ میلی‌ثانیه
برای هر مسیر به‌روز می‌شود. نشست بیکار مرتباً پایش نمی‌شود و به مسیر کپی داده
کار اضافه نشده است.

رمزنگاری، قالب پیام، اندازهٔ پنجره‌ها، سقف اتصال و مهلت جفت‌شدن تغییر نکرده‌اند.
استریم باز روی همان مسیر قبلی می‌ماند؛ این روش مشکل صف TCP یک استریم موجود را
از بین نمی‌برد. TCP Forward و KCP سیاست قبلی خود را نگه می‌دارند. برای سنجش
کیفیت، خطا و صحت داده را همراه با سرعت و تأخیر اتصال‌های تازه مقایسه کنید.

</div>

Last verified against Backpack v1.24.0 (2026-11-12).
