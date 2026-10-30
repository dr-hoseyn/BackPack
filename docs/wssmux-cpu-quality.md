# WSS mux CPU optimization with quality gates

This candidate changes compilation through Go PGO. It does not change TLS,
authentication, WebSocket masking, mux framing, compression, timers or windows.
It is an experiment until the paired CPU/quality workflow establishes a win.

## Profile provenance

`default.pgo` merges four 30-second CPU profiles from the already-deployed
`61eb57a48f9d3b012c7038400460915554f16d66` Linux/amd64 binary, built with
Go 1.26.10 without PGO. Two profiles were collected per role on 2026-10-30
starting around 15:31 UTC, during real high-concurrency WSS mux traffic.
Only CPU stacks were collected; no heap dumps, tunnel credentials or payloads
are in the profile. `go tool pprof -tags default.pgo` reports no labels.
The merged profile covers 120 seconds of wall sampling and 438.36 CPU seconds.

Profile SHA-256: `9b695104dbb3c6ac1ecce0addadb356cf0e0e82a9719df271c0d1285597f765b`.

The production profile has substantial syscall and runtime-scheduling costs;
WebSocket masking accounts for less than one percent in each role's first
sample. Optimizing masking alone is therefore not the initial production
candidate. A compiler speedup also cannot remove kernel work.

## Reproducible builds

Go automatically uses `default.pgo` for the main package. The release Makefile
and source installer build that package, so the same profile remains available
to subsequent builds. An explicit baseline is `go build -pgo=off`; an explicit
candidate is `go build -pgo="$PWD/default.pgo"`.

CPU profiles are not hardware binaries. Go can ignore unmatched functions on
other architectures or after source changes; the performance result must still
be measured for the intended architecture and workload. Do not assume the
same benefit for every carrier or future Go version. Refresh representative
profiles and repeat the comparison when the workload or toolchain changes.

The measurement binary must also be compiled with explicit `-pgo` flags:
`go test -c ./internal/e2e` does not automatically use the main package's profile.

## Acceptance before deployment

The CPU-quality workflow compares the same source with PGO off/on. It runs
race-checked authentication, binary framing, proxy, cancellation and buffer
ownership regressions. It alternates six independent pairs with 1,000 held-open
users, eight verified bulk workers, and the same 600 Mbit/s offered load.
Both binaries must carry identical verified payload counts.

The candidate must reduce the median paired CPU/GiB ratio by at least 3%,
with the upper 97.5th percentile of a fixed-seed paired-bootstrap median below
one. Equal-load goodput, sampled peak RSS and fresh-user p99 must also meet
the comparison's stated measurement tolerances. A CPU reduction obtained by
delivering fewer bytes is rejected. Saturated download must still exceed the
existing 600 Mbit/s floor. Separate isolated-network runs compare delay/loss
quality with automatic mux negotiation and pinned version 2.

These are conservative deployment decisions, not a claim that a finite test
proves no possible regression. The raw samples and rejected candidates are
retained as artifacts. No existing CI gate is relaxed. Never deploy just
because the PGO build compiles, and never infer production savings from an
isolated mask microbenchmark.

Last verified against Backpack v1.18.0.

<div dir="rtl">

پروفایل از CPU واقعی دو سمت گرفته شده است. کاهش CPU فقط وقتی پذیرفته می‌شود
که حجم داده یکسان باشد و تأخیر، حافظه، سرعت و صحت داده از آزمون‌های کیفیت عبور
کنند. تا پیش از نتیجهٔ مقایسه، این ساخت یک گزینهٔ آزمایشی است. تغییر قالب شبکه
یا ضعیف‌کردن TLS برای کم‌کردن CPU انجام نمی‌شود.

</div>
