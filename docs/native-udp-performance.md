# Native UDP sender cost

The sender now reuses bounded framing and encrypted wire buffers under its existing
write mutex. Fragment headers stay local to each call; AEAD and socket writes
consume the shared scratch before another sender can reuse it. The authenticated
header, cipher, counter, fragmentation, replay handling and wire bytes are unchanged.
The Noise library still allocates one nonce per encrypted fragment.

On Windows amd64, Go 1.26.9, Intel i7-11370H, two 200-ms samples per benchmark:

| Payload | Before time/op | After time/op | Before B/op | After B/op | Before allocs/op | After allocs/op |
|---|---:|---:|---:|---:|---:|---:|
| 64 bytes | 341–344 ns | 250–263 ns | 240 | 16 | 5 | 1 |
| 1000 bytes | 1151–1193 ns | 723–724 ns | 2224 | 16 | 5 | 1 |
| 1200 bytes | 1592–1643 ns | 1088–1098 ns | 2784 | 32 | 8 | 2 |
| 60000 bytes | 78603–88705 ns | 50792–51245 ns | 133440 | 960 | 240 | 60 |

These measure encrypted sender framing and fragmentation against an allocation-free
socket sink. They exclude network I/O, helpers, receiver CPU and application load;
they do not predict ISP throughput or whole-process RSS. Parallel sub-benchmarks
exercise contention on the same link. Reproduce with:

```sh
go test ./internal/nativeudp -run '^$' -bench '^BenchmarkNativeUDPEncryptedSend' -benchmem -count=5
```

Replay, unordered reassembly, drop, recovery and cancellation tests verify the
existing datagram behavior. An allocation-budget regression prevents per-fragment
framing allocations from returning. For CPU, observed RSS and verified throughput
through configured TCP protocol helpers at 1/8/32 concurrent connections, use
[the tunnel benchmark](../tools/tunnelbench/README.md). Real protocol rankings
require measurements on the deployment's two hosts; none are inferred here from
the sender microbenchmark.

## Engine framing and rekey audit (2026-10-22)

The engine also reuses its flow/target/payload frame. Concurrent backend reply
workers share one bounded frame under a mutex until the link has consumed it.
This removes a separate payload-sized allocation before encryption. Against
fork/main `00bf113e`, five matched Linux amd64 samples on Go 1.26.10 measured
the following at candidate `d4b6fe1e`:

| Engine payload | Base median | Candidate median | Base B/op | Candidate B/op | Allocs/op |
|---|---:|---:|---:|---:|---:|
| 1200 bytes | 1456 ns | 1205 ns | 1312 | 32 | 3 → 2 |

These use `BenchmarkNativeUDPEngineSend`, which includes engine framing in
addition to link encryption. They are separate from the older link-only table
above. A smaller B/op value measures allocation pressure, not total tunnel RAM.
The receiver checks backend queue capacity before copying the payload and
borrows the target bytes for existing-flow comparisons. A saturated backend
therefore adds no engine allocation for a discarded packet; the already
performed link authentication/decryption is a separate cost.

One datagram now keeps the same Noise key and peer for all its fragments, even
if the receive loop completes a rekey during its first socket write. The next
datagram adopts the new session. A real-socket regression delays a fresh Noise
response until the first fragment is sent, then verifies complete authenticated
delivery for 1001-, 3000- and 60000-byte messages and delivery on the new session.
The wire format, replay validation and unreliable datagram semantics are unchanged.

On Linux, reproduce the targeted checks and engine benchmark with:

```sh
go test -race ./internal/nativeudp -run 'TestNativeRekeyDelivers|TestNativeDatagramKeepsItsSession|TestConcurrentNativeEngine' -count=20 -timeout 3m
go test ./internal/nativeudp -run '^$' -bench '^BenchmarkNativeUDPEngineSend$' -benchmem -benchtime=1s -count=5
```

The [UDP audit workflow](../.github/workflows/udp-capacity-audit.yml) saves the
tested commit, base commit, Go version, reproduction results and benchmark
samples as artifacts. It also checks UDP pacing and saturated queues, session
ownership across client replacement, and repeated WS/WSS/mux/KCP churn with
the race detector. Successful loopback tests establish those tested behaviours;
deployment capacity still needs measurements on the actual two hosts and path.

<div dir="rtl">

## خلاصهٔ فارسی

برای پیام ۱۲۰۰ بایتی، در اندازه‌گیری محلی تخصیص حافظه از ۸ به ۲ مورد و از ۲۷۸۴
به ۳۲ بایت کاهش یافت. این سنجش فقط هزینهٔ رمزگذاری و بسته‌بندی فرستنده است، نه
سرعت اینترنت. ابزار tunnelbench مقایسهٔ واقعی تونل‌های تنظیم‌شده را با اتصال‌های
هم‌زمان، بررسی محتوای داده و نمونه‌برداری CPU و RSS محلی فراهم می‌کند.

در سنجش جداگانهٔ بسته‌بندی داخل Engine، تخصیص پیام ۱۲۰۰ بایتی از ۱۳۱۲ به ۳۲ بایت
کاهش یافت. بازبینی جدید، یکسان‌ماندن کلیدِ تمام تکه‌های یک پیام هنگام rekey و
بازسازی کامل پیام در گیرنده را نیز آزمایش می‌کند. صف پُرِ سرویس مقصد، بدون
ساختن کپیِ اضافی از بسته کنار گذاشته می‌شود و پس از تخلیه، جریان دوباره ادامه دارد.

</div>

*Last verified against Backpack v1.17.0, including the UDP/mux audit, 2026-10-26.
Link-only measurements: 2026-10-18. Engine measurements: candidate d4b6fe1e, 2026-10-22.*
