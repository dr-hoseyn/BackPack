# WSSMux carrier scheduling

WS Mux and WSS Mux now assign a waiting user to an available carrier using
socket congestion first, retaining readiness order among equally healthy
carriers. Previously every session competed for the same wake-up channel,
so a quiet-looking session with a backed-up TCP socket could take the next
user even when another physical session was draining normally.

On Linux the scheduler uses the existing TCP_INFO observer: queued bytes,
delivery rate, receiver window and retransmission state estimate the delay in
front of a new stream. Samples are cached for 50 ms per physical session;
there is no additional accounting on application reads or writes. TLS and the
deadline wrapper are unwrapped only to identify the observed TCP socket. All
traffic still passes through the original TLS/WebSocket connection.
Idle sessions are not polled. TCP Forward retains its existing 200 ms cache.

Admission visits carriers in their readiness order, returning the first with
zero pressure. This preserves the healthy shared-channel wakeup ordering
without treating quiet long-lived streams as evidence of congestion. If every
available carrier has pressure, it selects the smallest score and retains
readiness order for ties. The intrusive readiness list reuses the per-carrier
waiter; it adds no allocation for each selection or wakeup. Returning after a
successful admission moves a worker behind those already waiting.

An exhaustive regression compares this early return against full
congestion/readiness ordering across 4096 six-carrier pressure combinations.
A separate regression checks that many quiet users do not displace an older
healthy waiter and that redundant registration does not move a waiting worker.

Other platforms, unavailable kernel counters and unsupported wrappers fall
back to readiness-order selection. No carrier is discarded just for having a
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


A [follow-up measurement](benchmarks/wssmux-outcomes-2026-11-12.json) includes
the closed-waiter fix and records every attempted probe, including failures,
as well as demand skipped at the four-probe concurrency cap. All 477 scheduled
probes per revision were attempted; none were skipped. There were three
failed fresh probes before and one after. Median per-run p99 including failed
attempts was 173.7 and 149.8 ms, and p95 was 123.8 and 98.0 ms. Median measured
application throughput was 19.901 and 19.878 Mbit/s at the fixed offered load;
bulk and corruption errors remained zero. Candidate run 3 had a 249.7 ms p99,
worse than any baseline run. These three short runs per revision are promising
but do not establish an absence of quality regression: this scheduler remains
a draft. The older artifact uses successful-only latency and a different
quantile definition, so its latency samples must not be pooled with these.
The follow-up does not measure the separate shared-channel fix in PR #68.

The test host exposes one vCPU and no AES, PCLMULQDQ, SSSE3, SSE4.1, AVX2
or BMI2 feature flags. A separate [CPU profile](benchmarks/wssmux-cpu-profile-2026-11-12.json)
of PR #69's existing resource fixture recorded generic ChaCha20 hot paths
and syscall overhead while carrying a fixed 200 Mbit/s offered load. This
profile combines both engines, the echo backend and the load generator; it
does not compare this scheduler. Its CPU and latency measurements cannot be
generalized to an eight-core host with crypto acceleration. The owner's
four-vCPU host exposes these features. A subsequent
[profile on that host](benchmarks/wssmux-modern-cpu-profile-2026-11-12.json)
used the identical binary with GOMAXPROCS=2 for the combined test process:
at the same offered load it reported 200.0 Mbit/s, 27.04 CPU seconds per GiB
and fresh-probe p99 of 1.598 ms. Its sampled hot paths include accelerated
AES-GCM, with 58.89% of CPU samples in Syscall6. This is a host diagnostic;
different hardware and CPU limits prevent attributing these differences to
a software optimization. Neither profile evaluates this scheduler.

A separate [same-host scheduler comparison](benchmarks/wssmux-modern-outcomes-2026-11-12.json)
ran three trials per revision on the four-vCPU host with the earlier 20 Mbit/s
workload and six-second carrier pause. Every scheduled probe was attempted:
baseline had three failures in 477, candidate had one. Median per-run p99
including failed attempts was 32.509 versus 16.050 ms, and application goodput
was 19.972 versus 19.970 Mbit/s. Bulk/corruption errors were zero, and all
32 silent streams remained usable. However, candidate run 2 had p99 205.516 ms,
worse than every baseline run; median p95 was 2.971 versus 2.681 ms. These
results do not establish no regression or justify a general speedup claim.
A [same-host no-fault control](benchmarks/wssmux-modern-control-2026-11-12.json)
attempted all 477 probes per revision without bulk, corruption or probe errors.
Median per-run p99 was 12.609 versus 7.800 ms, but median p50 increased from
1.727 to 1.926 ms and p95 from 2.552 to 2.866 ms. Those typical-latency
regressions matter even with a better p99: the scheduler remains a draft.
Both comparisons measure code 9a2e747 and are separate from the CPU profiles.
They predate the admission-sampling shortcut above and are not measurements
of that follow-up change.

The [shortcut's separate no-fault comparison](benchmarks/wssmux-shortcut-control-2026-11-12.json)
measures code 14a9105. All 477 probes per revision were attempted with zero
bulk, corruption or probe errors, and the silent streams remained usable.
Median p50 was 1.845 versus 1.878 ms, p95 was 2.608 versus 3.026 ms, and p99
was 7.217 versus 6.392 ms. Goodput stayed near the fixed offered 20 Mbit/s.
The p50 difference is small in this run, but p95 remains higher; the shortcut
does not establish latency parity. The earlier fault results do not measure
this revision. Raw samples are kept separate and the PR remains a draft.

## خلاصهٔ فارسی

<div dir="rtl">

در WS Mux و WSS Mux، اتصال تازه ابتدا به نشستی سپرده می‌شود که گرفتگی TCP
کمتری دارد؛ میان حامل‌های سالم، ترتیب آماده‌شدن workerها حفظ می‌شود و تعداد
استریم‌های ساکت، حامل سالم قدیمی‌تر را کنار نمی‌زند. اطلاعات گرفتگی فقط هنگام نیاز به پذیرش اتصال، حداکثر هر ۵۰ میلی‌ثانیه
برای هر مسیر به‌روز می‌شود. نشست بیکار مرتباً پایش نمی‌شود و به مسیر کپی داده
کار اضافه نشده است.

در مسیر سالم، نشست‌ها به ترتیب آماده‌شدن بررسی می‌شوند و اولین فشار صفر
انتخاب را کامل می‌کند. اگر همهٔ نشست‌ها فشار داشته باشند، کم‌فشارترین نشست
انتخاب می‌شود و در امتیازهای برابر، ترتیب آمادگی حفظ می‌شود. اندازه‌گیری‌های پیش و پس از اصلاح، با نام نسخه‌ها
جداگانه ثبت شده‌اند؛ بالاتر رفتن p95 هنوز منتفی نشده است.

رمزنگاری، قالب پیام، اندازهٔ پنجره‌ها، سقف اتصال و مهلت جفت‌شدن تغییر نکرده‌اند.
استریم باز روی همان مسیر قبلی می‌ماند؛ این روش مشکل صف TCP یک استریم موجود را
از بین نمی‌برد. TCP Forward و KCP سیاست قبلی خود را نگه می‌دارند. برای سنجش
کیفیت، خطا و صحت داده را همراه با سرعت و تأخیر اتصال‌های تازه مقایسه کنید.

سرور اندازه‌گیری فعلی فقط یک هستهٔ مجازی دارد و قابلیت‌های شتاب‌دهی رمزنگاری
به آن ارائه نشده‌اند. نتیجهٔ سرعت و تأخیر این سرور را نمی‌توان به سرور هشت‌هسته‌ای
تعمیم داد. پروفایل ثبت‌شده، مجموع دو سمت تونل و ابزار تولید بار را اندازه می‌گیرد.

</div>

Last verified against Backpack v1.24.0 (2026-11-12).
