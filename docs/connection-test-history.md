# Connection Test history

Terminal and web panel Connection Tests save measurements automatically on each server after a run.
Open **Connection Test → History**, or use:

```sh
backpack test history
backpack test compare
backpack test history --json
backpack test compare --json
```

The most recent 100 runs are kept under `/etc/backpack/connection-tests/`.
Times are UTC. The directory is private (0700) and each sample is written as an
independent atomic 0600 JSON file, so overlapping test processes cannot overwrite
one another. A failed history write warns without changing the test verdict.
Corrupt or oversized samples produce a warning while valid samples stay readable.
No setup links, tokens, passwords, configurations or raw diagnostic errors are saved.
Server IPs, the peer IP when available, SNI names and measured results are retained.

Comparison separates Quick/Full depth, the local role, preset, Iran IP, Kharej IP when known,
IP family, reverse/direct/spoof direction, protocol and SNI candidate. Cancelled
runs and paths without a completed measurement are shown in history but excluded
from comparison. Skipped, still-running and screened (speed not measured) rows are also excluded.
Quick runs are labelled as 30-second observations; older saved runs are treated
as Full when grouping comparisons.

Loss is calculated from the total attempted and received echoes, not an average
of percentages. RTT and per-run jitter are weighted by received echoes. Download
and upload use the median of healthy completed transfers with both speeds measured;
`speed_samples` states how many contributed. A zero-speed measurement is missing
throughput evidence, not a measured zero-throughput link. `steady/runs` describes
these short test samples, never long-term uptime. Compare repeated samples over
representative hours before choosing a carrier; the program does not schedule
network traffic automatically.

Existing stored configurations and coordinator links are unchanged. New peers
include per-protocol jitter in the result; older peers omit it, and their jitter is shown as unmeasured. The panel Network page reads the same history and comparisons through authenticated APIs.

<div dir="rtl">

## خلاصهٔ فارسی

نتایج تست ترمینال و پنل خودکار ذخیره می‌شوند و از گزینهٔ History یا دستورهای بالا قابل
مشاهده و مقایسه‌اند. حداکثر ۱۰۰ اجرا نگهداری می‌شود. تست لغوشده در مقایسه حساب
نمی‌شود؛ مسیر، خانوادهٔ IP، تنظیمات و دامنهٔ SNI متفاوت جدا می‌مانند. نسبت اجراهای
موفق، درصد آپ‌تایم نیست. لینک اتصال، رمز و خطای خام ذخیره نمی‌شوند.

</div>

*Last verified against Backpack v1.14.2, 2026-10-18.*
