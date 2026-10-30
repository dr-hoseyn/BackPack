# WSS mux CPU optimization with quality gates

This experiment adds opt-in aggregation of ready SMUX data frames in framed
WebSocket mux connections. It is disabled by default. Enable separately on
both roles with `wsmux_write_batch = true` only after workload-specific quality
and CPU comparisons pass. One upgraded role can interoperate with an older
peer: the ordered SMUX byte stream and protocol negotiation stay compatible.
Legacy unframed connections and all other carriers keep the upstream send loop.

## Bounded synchronous writes

The sender joins only requests already available in SMUX's existing shaper.
There is no batching timer, extra queue or asynchronous write acknowledgement.
A lone interactive write is sent immediately. Each group contains at most
16 frames and 64 KiB, except an individual maximum-size upstream frame which
is still sent intact. Controls leave separately; round-robin stream ordering
is preserved. Borrowed payloads are consumed synchronously, per-request partial
write counts exclude headers, and idle sessions release old payload references.

TLS, authentication, WebSocket masking, compression, mux windows and deadlines
are unchanged. This does change WebSocket message grouping when enabled; it
is not a claim of improved resistance to traffic classification. Correct
framing and authenticated negotiation remain required.

The small extension lives in a pinned local copy of `github.com/xtaci/smux
v1.5.57`, including its tests and MIT license. See
`third_party/smux/BACKPACK_PATCH.md` for provenance and update instructions.
Future upstream updates require rebasing the extension and rerunning both the
upstream tests and BackPack quality checks. Normal BackPack binary updates
remain unchanged; config editing preserves the optional flag.

## Rejected compiler experiment and profile provenance

Production-trained PGO was tested first and rejected. In workflow 41082850400,
six alternating equal-load pairs showed a median paired CPU/GiB change of
-1.22%, below the required 3% reduction. On the delayed/lossy automatic-mux
fixture, median goodput fell from 76.7 to 73.6 Mbit/s. The candidate was not
deployed. Its result is retained; the batching comparison must pass the same
gates without PGO.

`profiles/wssmux-production.pgo` retains the diagnostic production profile;
its non-default name prevents automatic use in ordinary builds. It merges four
30-second CPU profiles from deployed commit
`61eb57a48f9d3b012c7038400460915554f16d66`, Go 1.26.10 Linux/amd64 without PGO.
Two were collected per role on 2026-10-30 starting around 15:31 UTC, during real
high-concurrency WSS mux traffic. Only CPU stacks were collected: no heap dumps,
tunnel credentials, payloads or pprof labels. Sampling covers 120 seconds of
wall time and 438.36 CPU seconds. Profiling was then disabled and the original
production configuration restored.

Profile SHA-256: `9b695104dbb3c6ac1ecce0addadb356cf0e0e82a9719df271c0d1285597f765b`.
Syscalls and scheduling dominate these samples; WebSocket masking accounts for
less than one percent in each role's first sample. A masking microbenchmark
therefore cannot establish a useful end-to-end gain for this workload.

## Acceptance before deployment

The CPU-quality workflow compares the deployed base revision against the
candidate with batching enabled, using the identical resource fixture and
explicit `-pgo=off` in both builds. It runs the pinned SMUX upstream suite and
benchmarks plus race-checked authentication, framing-proxy, cancellation,
concurrent streams and borrowed-buffer regressions.

Six independent pairs alternate process order with 1,000 held-open users,
eight verified bulk workers and the same 600 Mbit/s offered load. Both must
carry identical verified payload counts. The candidate must reduce median
paired CPU/GiB by at least 3%, with the upper 97.5th percentile of a fixed-seed
paired-bootstrap median below one. Goodput, peak sampled RSS and fresh-user
p99 must also pass the comparator's stated measurement tolerances. Delivering
fewer bytes cannot qualify as a CPU improvement.

Saturated downloads must still pass the existing 600 Mbit/s floor. Separate
isolated-network runs check delay/loss with automatic mux negotiation and
pinned version 2. Results from both modes are retained even if one fails.
No existing quality gate is relaxed. These finite measurements are conservative
deployment decisions, not proof against every possible regression. Keep this
option off if any quality check fails, even when CPU improves.

Last verified against Backpack v1.18.0.

<div dir="rtl">

## خلاصهٔ فارسی

اولویت، کیفیت و صحت داده است. تجمیع فقط برای فریم‌های آماده و بدون تایمر انجام
می‌شود و پیش‌فرض خاموش است. کاهش CPU باید همراه با حفظ سرعت، تأخیر و مصرف حافظه
در آزمون‌های برابر اثبات شود. آزمایش PGO به دلیل نتیجهٔ ناکافی و افت سرعت در
مسیر دارای تأخیر رد شد و روی سرویس فعال نصب نشد. پروفایل آن صرفاً برای تحلیل
نگهداری می‌شود و در ساخت عادی خودکار فعال نیست.

</div>
