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

<div dir="rtl">

## خلاصهٔ فارسی

برای پیام ۱۲۰۰ بایتی، در اندازه‌گیری محلی تخصیص حافظه از ۸ به ۲ مورد و از ۲۷۸۴
به ۳۲ بایت کاهش یافت. این سنجش فقط هزینهٔ رمزگذاری و بسته‌بندی فرستنده است، نه
سرعت اینترنت. ابزار tunnelbench مقایسهٔ واقعی تونل‌های تنظیم‌شده را با اتصال‌های
هم‌زمان، بررسی محتوای داده و نمونه‌برداری CPU و RSS محلی فراهم می‌کند.

</div>

*Last verified against Backpack v1.11.0, 2026-10-18.*
