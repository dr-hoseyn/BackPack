# WSS Mux resource use

TCP relay directions start with a pooled 16 KiB buffer. A full read grows that
direction immediately to the existing pooled 64 KiB bulk buffer. No bytes are
held back to gather a larger write, and no application deadline changes.
Directions that never fill the small buffer retain one quarter of the previous
copy-buffer memory. This is a buffer-size calculation, not a claim that process
RSS falls by 75 percent: mux queues, TLS, stacks, sockets and other state remain.
Buffers held by a failed mux write are never recycled into another transfer.
The TCP relay also reuses its caller for one direction instead of keeping a
third goroutine parked beside two copies. The existing cancellation callback
interrupts idle reads, and both directions are joined before releasing quotas.

## Physical session growth and retirement

WS Mux and WSS Mux sample the number of sessions with actual open streams once
per second. While users are present, the established request and throughput
growth policy is retained: limiting physical TCP paths can shift loss-recovery
queues onto fresh users. Occupancy prevents stale byte-rate growth after all
streams have ended and preserves occupied sessions when lowering the repair
target. Warm capacity and the existing automatic protocol choice are retained;
new-user bursts still use the existing pending-dial ceiling.

When both peers support retirement, the client lowers its repair target after
30 seconds without forwarded traffic or new session demand, retaining at least `connection_pool`
and all occupied sessions. The server closes a surplus session only after it
has no streams or relay workers for 60 seconds and the tunnel's forwarded
traffic counters have been unchanged for a full 30-second observation period.
The observation begins when an eligible idle session first checks, so release
can take about 90–95 seconds after the last user finishes. Warm capacity stays
available during ongoing transfers. The server's admission loop
makes that decision, so it cannot race a newly opened stream. Simultaneous
retirements reserve their removals under the existing counter lock and cannot
cross the advertised pool floor. A silent open connection is still an occupied
stream and is preserved. KCP and TCP Mux retain their current retirement policy.

## Version agreement and older peers

The authenticated HTTP upgrade on a WS Mux/WSS Mux **data connection** now
offers supported smux versions. Two automatic peers retain version 1. An
explicit version 2 on either new peer negotiates version 2, which enforces the
configured per-stream flow-control window. Explicit version 1 is honored;
incompatible explicit choices are rejected during setup. Existing presets
already pin version 2. A blanket automatic upgrade was rejected after the
lossy-path comparison showed worse fresh-user tail latency.
The control-channel framing, TLS ClientHello and session-bound credential stay
unchanged.

An older peer sends no negotiation headers. That connection retains the legacy
static choice: auto uses version 1; an explicit version uses that number. Both
ends of an explicitly pinned legacy connection must still match. Idle
retirement is enabled only when the pool-floor extension is acknowledged, so
an older client is not asked to understand retirement. Upgrade peers in either
order; upgrading both enables the new session behavior.

Version 2 makes `mux_streambuffer` effective. A hand-written small stream window
can cap throughput on a high-RTT route. Existing explicit window sizes are
preserved. Choose a matching preset on both ends or size the window from the
needed per-stream rate and round-trip time; do not shrink all mux/socket
windows just to obtain a smaller RAM figure. Leave `so_pin_tcp = false` unless
measurements justify fixed TCP buffers.

## Measure before tuning the garbage collector

Compare at the same offered load, CPU allocation, runtime settings and helper
configuration. Record idle, peak and post-peak heap and RSS, socket memory,
CPU-seconds per application GiB, fresh-user p99 latency and failed transfers.
Do not interpret lower CPU caused by lower delivered throughput as an
improvement. Loopback measurements establish regressions, not WAN capacity.

The `WSS Mux resource verification` workflow alternates five independent
baseline (`v1.16.0`) and candidate processes on one runner. Each holds 1,000
verified idle TCP streams while eight workers transfer random payloads and
new users send probes. Both revisions use smux 2, as existing presets do, to
isolate resource policy from protocol changes. Every echo is checked byte-for-byte. CPU covers the
combined client/server process; echo goodput counts one direction's application
bytes once. The report includes GC cycles and pause time. A separate real
60-second retirement test keeps one user silent, verifies the pool returns to
its configured floor and then transfers more bytes on that same connection.
The broader throughput workflow independently verifies one-direction capacity.
An additional comparison uses disposable network namespaces with 40 ms delay
in each direction, 0.1 percent packet loss and a 200 Mbit/s queue, applied only
to the actual tunnel port at receiver ingress through IFB. The isolated
interfaces use MTU 1500 with TSO/GSO/GRO disabled and a fixed loss seed. This
avoids the TCP Small Queues and giant-loopback-packet artifacts described in
the [tc-netem manual](https://www.man7.org/linux/man-pages/man8/netem.8.html).
It keeps the application/backend hops local and
compares three independent processes per version with 16 idle streams in each
of two modes: automatic version selection and explicitly pinned smux 2.

Reproduce on Linux:

```sh
GOMAXPROCS=4 BP_RESOURCE_BENCHMARK=1 \
  go test ./internal/e2e -run '^TestWSMuxResourceProfile$' -count=1 -v
BP_RESOURCE_RETIREMENT=1 \
  go test ./internal/e2e -run '^TestWSMuxPoolRetiresAfterLoadWithoutDisconnectingQuietUser$' -count=1 -v -timeout 3m
```

For a production workload, temporarily set `pprof = true` and capture a CPU
profile and heap during representative traffic. Server pprof is on
`127.0.0.1:6060`, client pprof on `127.0.0.1:6061`; use SSH forwarding to reach
them. Keep these endpoints on loopback. Separate GC work from TLS/crypto,
syscalls, allocation and scheduler work before choosing a tuning change.

No `GOGC` or `GOMEMLIMIT` override is enabled by this release. `GOMEMLIMIT` is a
soft budget for one Go runtime, not a hard RSS limit or a limit on all tunnel
processes. Leave room for kernel socket memory, helpers and peak live data.
Lowering the limit below live demand increases collection work; lowering GOGC
also trades CPU for RAM. Preserve runtime defaults unless a same-load profile
and a p99/throughput comparison justify a per-service setting.

## خلاصهٔ فارسی

<div dir="rtl">

بافر هر جهت TCP از ۱۶ کیلوبایت شروع می‌شود و پس از یک خواندن کامل، برای انتقال
حجیم به ۶۴ کیلوبایت رشد می‌کند. صرفه‌جویی ۷۵ درصدی مربوط به همین بافرِ جهت‌های
کوچک و بیکار است؛ کاهش کل RAM به بار، TLS و سوکت‌ها بستگی دارد.

در WS Mux و WSS Mux، نشست‌های اضافی فقط وقتی هیچ استریم یا کاربر باز ندارند و
۶۰ ثانیه بیکار بوده‌اند بسته می‌شوند. حداقل پول حفظ می‌شود و کاربر ساکت قطع
نمی‌شود. هر دو سمت جدید، نسخهٔ smux را هنگام handshake هماهنگ می‌کنند؛ ارتباط
با نسخهٔ قدیمی از انتخاب قبلی استفاده می‌کند. برای فعال‌شدن بازنشستگی امن، هر
دو سمت را ارتقا بده. اندازهٔ پنجره‌ها و تنظیم GC را بدون سنجش سرعت و تأخیر کم
نکن؛ تست این صفحه مصرف CPU و حافظه را همراه با صحت داده و تأخیر کاربران جدید
مقایسه می‌کند.

</div>

Last verified against Backpack v1.17.0 (2026-10-27).
