# Connection Test coordinator in v1.20.0

Both peers must run v1.20.0 or newer. Start a new test after upgrading. Single
endpoint links use version 3; two-path IPv4/IPv6 suite links use version 4.
Versions 1/2 used plaintext control traffic and are rejected before dialing.
Existing tunnel services and their wire formats are unchanged by this switch.

Control packets use ChaCha20-Poly1305 with a key derived from the random test
secret. Requests use fresh nonces; replies also authenticate their request's
nonce and their direction, preventing substitution between progress polls.
Automatic-setup credentials are encrypted inside that channel. The short link
itself grants access to the test and must still be kept private.

TCP uses a bounded four-byte length prefix. Requests are capped at 8 KiB;
responses are capped at 256 KiB. UDP remains one bounded message. TCP starts
first and UDP joins after 150 ms, so an unresponsive TCP path does not impose
its entire timeout before each progress query. Cancellation closes the sockets.
Verdict and stop acknowledgements still wait until the complete reply is sent.
IPv4/IPv6 PMTU probes include encryption overhead in their datagram-size budget.

The existing verified upload/download speed tests, kernel suite, short kernel
Quick profile, test history and automatic-setup transaction are retained. Three
independent verified connections and p95/observation evidence augment persistent
traffic checks. Missed sampling slots count as failed probes rather than a burst
of catch-up traffic. Probe failures are application-level evidence, not a raw
packet-loss measurement. SSH's temporary forwarding-only endpoint is cleaned up
with the test and cannot be promoted as a production sshd login.

<div dir="rtl">

## خلاصهٔ فارسی

هماهنگ‌کنندهٔ Connection Test رمزنگاری و احراز اصالت می‌شود؛ پاسخ TCP کامل و محدود خوانده می‌شود و پس از ۱۵۰ میلی‌ثانیه مسیر UDP هم آزمایش می‌شود. هر دو سرور را ارتقا بده و لینک جدید بساز. لینک تکی نسخهٔ ۳ و لینک دو مسیر نسخهٔ ۴ است. سنجش سرعت دوطرفه و تست‌های کرنلی موجود حفظ شده‌اند.

</div>

*Last verified against Backpack v1.20.0, 2026-11-01.*
