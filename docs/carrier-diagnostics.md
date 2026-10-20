# TUIC and Hysteria2 diagnostics

*Last verified against Backpack v1.14.1, 2026-10-18.*

<div dir="rtl">

خلاصهٔ فارسی: دستور `backpack diagnose -c CONFIG --json` را در سمت خارج اجرا کنید. برای TUIC و Hysteria2، مرحلهٔ حل آدرس، پاسخ UDP و QUIC، اعتبار گواهی، اجرای helper، اثبات احراز هویت و انتقال واقعی داده جداگانه گزارش می‌شوند. عدم پاسخ UDP به‌تنهایی به معنی فیلتر بودن یا اشتباه بودن رمز نیست. سرویس موجود بازراه‌اندازی نمی‌شود و محتوای رمزها و لاگ خام در گزارش قرار نمی‌گیرد.

</div>

Run on Kharej with its existing Reverse client config:

```sh
sudo backpack diagnose -c /etc/backpack/client.toml --json
sudo backpack diagnose -c /etc/backpack/automatic.toml --path tuic
```

The command is bounded to two minutes, exits 4 when a path does not verify, and
uses disposable private helpers. It does not edit configuration or restart the
running tunnel. A managed bundle tests its configured Hysteria2/TUIC paths; a
single carrier config tests that carrier. `--path` uses the configured path name.

1. Validate local settings and private certificate trust.
2. Resolve the public endpoint, probing at most four resolved addresses.
3. For TUIC/plain Hysteria2, independently attempt QUIC with verified TLS and
   `h3` ALPN, counting sent/received UDP packets. TLS trust/name failures have
   separate codes. A QUIC handshake does not authenticate a TUIC account.
   Salamander listeners skip this incompatible plain-QUIC probe.
4. Start the actual pinned sing-box helper with its normal authentication and
   certificate verification; establish a route only to the configured private
   Reverse listener.
5. Verify a token-bound peer proof and 16 KiB in each direction. Only this proof
   confirms the authenticated route. No control seat or forwarded backend is used.
6. If enabled, verify authenticated native UDP through the actual helper too.

Both peers need a build supporting the diagnostic probe for single managed
carriers. Existing adaptive pools already support this wire opcode. An older
peer, wrong token, helper authentication failure or interrupted payload can leave
the proof unverified; this is not automatically called network filtering.

The independent QUIC probe has a different fingerprint from the pinned helper.
Its failure does not prevent testing the actual helper. An authenticated payload
can therefore pass despite an independent probe warning. Packet counts describe
only this independent probe, not the helper's traffic or the remote firewall.

Explicit authentication/certificate/timeout messages from a bounded private log
tail are mapped to fixed report codes. Raw helper logs and configuration secrets
are excluded; the temporary log is removed after helper shutdown. For an unknown
error the report keeps authentication unconfirmed instead of guessing a cause.
