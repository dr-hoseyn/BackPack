# Updates & rollback

The **Update** menu detects a newer GitHub release and installs the prebuilt
`backpack_linux_<arch>.tar.gz`.

## How it downloads

**Direct from GitHub, or through a tunnel peer** when this server cannot reach
GitHub itself. Both paths terminate TLS at GitHub, so the download is verified
against the release's published **SHA-256**.

**An archive that cannot be verified is refused, not installed.** A server that
can reach neither path installs offline instead — see the offline install
section in the main README.

## Safety net

Before touching anything, Update saves a **restore point**. After installing it
health-checks the result and **rolls back by itself** if anything fails to come
back up. You can also roll back on demand from **Update → Restore points**.

### Verify traffic after an update

Service startup alone does not establish that a tunnel carries data. To make
rollback depend on a real HTTP transfer through a forwarded port, provision a
small, dedicated HTTP health endpoint on the far server. Give it a stable,
nonempty response body, for example `backpack-health-v1` followed by a newline.
The endpoint must accept GET requests with query parameters.

On a Reverse server or Direct Iran edge, a mapping such as
`127.0.0.1:18080=127.0.0.1:18081` can carry a health endpoint listening on port
18081 on the far server. Configure that mapping on the tunnel first, then create
`/etc/backpack/update-health.json` on the entry server:

```json
{
  "grace_seconds": 45,
  "checks": [
    {
      "tunnel": "my-tunnel",
      "url": "http://127.0.0.1:18080/health",
      "expected_body": "backpack-health-v1\n"
    }
  ]
}
```

Replace `my-tunnel` with the installed tunnel's name. The response must match
`expected_body` byte for byte, including newlines. Use a dedicated endpoint;
ordinary forwarded application ports are never probed automatically.

Online and offline Update use this policy in the same way:

- Before installation, every configured check must pass. An existing endpoint
  or path failure stops the update before the binary is replaced.
- After restart, each check opens a fresh HTTP connection through its named
  tunnel. A failed check retries within one shared grace period (default 45
  seconds, maximum 120). If any check still fails, Update restores the snapshot.
- HTTP 200 alone is insufficient: the complete response body must match. Requests
  bypass environment proxies, include a fresh cache-busting nonce, refuse
  redirects, retain normal HTTPS certificate validation and have bounded time
  and response sizes. URLs and response bodies are not copied to update logs.
- A URL must use a literal loopback address and an explicit local port owned by
  that tunnel's forwarding configuration. Unknown tunnels and malformed policy
  files stop the update. Up to 32 tunnels can be checked, once per tunnel.

This also works on L3/AmneziaWG sides with local forwarded ports; their mapping
must target the **far peer's tunnel address**, not this machine's loopback.
Reverse clients and Direct Kharej origins have no local entry listener; raw
Reverse UDP owns no TCP entry. These cannot use this check. A TCP/HTTP check does not verify native UDP forwarding,
every load-balanced backend, or both ends' installed versions.

The policy is local to this server, survives config edits and is included in
restore points. Without a configured check, Update explicitly reports that
payload verification is unavailable for that tunnel and checks service startup
as before. It does not call an idle tunnel broken. These checks apply when the
**updater process initiating the update** includes this feature; an older updater
cannot gain a new safety check midway through its own upgrade.

## Channels

Follow **stable** (default) or **beta** under **Update → Release channel**.

## Upgrading a very old install

From a clone-based install (≤ v1.2.0): run Update once; after that it is
release-based.

---

<div dir="rtl">

## خلاصهٔ فارسی

منوی **Update** ریلیز جدیدتر گیت‌هاب را پیدا و نصب می‌کند.

**چطور دانلود می‌کند:** مستقیم از گیت‌هاب، یا **از طریق یک تونل** وقتی خود این
سرور به گیت‌هاب دسترسی ندارد. هر دو مسیر TLS را در گیت‌هاب terminate می‌کنند، پس
دانلود با **SHA-256** منتشرشدهٔ ریلیز تأیید می‌شود.
**آرشیوی که تأیید نشود نصب نمی‌شود، رد می‌شود.**

**تور ایمنی:** قبل از دست زدن به چیزی یک **restore point** ذخیره می‌کند، بعد از
نصب نتیجه را health-check می‌کند و **اگر چیزی بالا نیامد خودش برمی‌گردد عقب**.
دستی هم می‌شود از `Update → Restore points` برگشت.

**تأیید عبور داده:** با فایل محلی `/etc/backpack/update-health.json` می‌توان یک
endpoint سلامت HTTP روی پورت فورواردشدهٔ هر تونل تعریف کرد (نمونهٔ بالا).
پاسخ باید دقیقاً با `expected_body` برابر باشد. قبل از نصب، شکست آزمون جلوی
آپدیت را می‌گیرد؛ بعد از ری‌استارت، شکست مداوم تا پایان مهلت تعیین‌شده باعث
بازگردانی restore point می‌شود. بدون این تنظیم، فقط بالا آمدن سرویس بررسی
می‌شود و تأیید نشدن ترافیک هم صریحاً در گزارش نوشته می‌شود.

**کانال:** پیش‌فرض **stable**؛ اگر بخواهی pre-release‌ها را هم بگیری، از
`Update → Release channel` روی **beta** بگذار.

سروری که به هیچ‌کدام دسترسی ندارد، [آفلاین نصب می‌کند](install.md).

</div>

---
[← Back to the docs index](README.md)

---

*Last verified against Backpack v1.20.0*
