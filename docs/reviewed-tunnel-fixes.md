# Reviewed tunnel fixes

This change preserves tunnel wire formats and configuration defaults.

Multipath UDP retains ingress identity until Noise authenticates the packet.
Only authenticated packets teach per-path destinations. Handshake replies use
the ingress socket; reconstructed FEC packets cannot authenticate the source of
a missing shard. TCP clients try all endpoints after control-handshake failures.
Explicit refusals and legacy negotiation retain their existing behavior.
Multipath deadlines wake pending reads and cover both read and write directions.
Cached init replies require the original authenticated message and announcement.
Malformed FEC lengths are rejected before group mutation; structurally valid
forged shards still require a compatible authentication design (issue #44).

Issues #45-#48 and #50 remain performance/design work. Removing reservations may
steal stopped service ports. Smaller UDP buffers may truncate valid datagrams.
Batching and asynchronous pacing need bounded queues, integrity tests and Linux
comparisons. No throughput gain is claimed for this correctness change.

```sh
go test -race ./internal/tunnel/l3 ./internal/client/transport -run 'TestReviewed|TestFEC|TestMultipath|TestTunnelCarries' -count=3 -timeout 5m
```

Full CI separately checks compatibility, allocation budgets, throughput and TUN.

---

## خلاصهٔ فارسی

این اصلاحات قالب بسته‌ها و پیش‌فرض تنظیمات را تغییر نمی‌دهند. مقصد UDP پس از
احراز هویت یاد گرفته می‌شود و فیل‌اوور TCP پس از شکست دست‌دهی آدرس‌های دیگر را
امتحان می‌کند. اصلاح ساختار FEC حفاظت کامل در برابر بسته جعلی معتبرنما نیست.
تغییر بافر، batching، pacing و رزرو پورت نیازمند آزمون و اندازه‌گیری جداست.
افزایش سرعتی برای این اصلاحات ادعا نمی‌شود.
