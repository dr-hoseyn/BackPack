# Monitor service

The watchdog, Telegram bot and alerts run in `backpack-monitor.service`.
This service is independent of the interactive CLI; closing an SSH session does
not stop monitoring. The CLI installs it on launch and updates refresh its unit.
Use **Manage → Health Check** to inspect it, or:

```sh
systemctl status backpack-monitor
journalctl -u backpack-monitor -n 50
```

No browser listener is created. Optional per-port traffic accounting writes its
usage log without an HTTP page. On upgrade, the retired dashboard service is
stopped and disabled before its unit is removed.

<div dir="rtl">

## خلاصهٔ فارسی

واچ‌داگ، ربات تلگرام و هشدارها در سرویس مستقل `backpack-monitor.service` اجرا
می‌شوند. بستن SSH مانیتورینگ را متوقف نمی‌کند. پنل و پورت مدیریتی وب وجود ندارد.

</div>

Last verified against Backpack v1.15.1 (2026-10-23).
