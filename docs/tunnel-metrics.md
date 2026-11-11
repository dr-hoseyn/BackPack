# Tunnel Metrics

**Manage → Tunnel Metrics** reports current connection health, helper readiness,
pool size, resource use and transport diagnostics. KCP diagnostics include lost,
duplicated and retransmitted segments and FEC recovery.

Usage totals, volume quotas and per-port usage logs are removed. The engine
keeps in-memory counters for instantaneous speed, adaptive pool sizing and stall
detection. These counters are not saved across restarts. History records uptime
observations without byte volumes. Legacy `sniffer` and `sniffer_log` settings
are accepted for configuration compatibility.

Heap diagnostics use `runtime/metrics` rather than stopping all goroutines with
`ReadMemStats`. Descriptor enumeration reads bounded batches of names so the
temporary list does not grow with the total number of open sockets.

<div dir="rtl">

## خلاصهٔ فارسی

آمار مصرف حجمی، سهمیهٔ حجم و ثبت مصرف هر پورت حذف شده‌اند. اندازه‌گیری لحظه‌ای
سرعت برای تنظیم pool و تشخیص توقف انتقال در حافظه انجام می‌شود. تاریخچه فقط
وضعیت اتصال را ثبت می‌کند و حجم ترافیک را ذخیره نمی‌کند. تنظیمات قدیمی
`sniffer` و `sniffer_log` برای سازگاری خوانده می‌شوند.

</div>

---
[← Back to the docs index](README.md)

---

*Last verified against Backpack v1.24.0*
