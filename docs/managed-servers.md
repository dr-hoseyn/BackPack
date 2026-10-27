# Manage servers over SSH

Open the CLI on the host you want to manage:

```sh
ssh root@YOUR_SERVER
sudo backpack
```

Use **Setup Iran** on the entry server and paste its setup link under **Setup
Kharej → Setup Link** on the peer. Connection Test also supports selecting a
successful measured tunnel and configuring both ends automatically from either
server; see [automatic setup after a test](connection-test-setup.md).

For scripts, `backpack --help` and the commands documented in the CLI reference
provide the supported management surface. `backpack node exec -` is the
stdin/stdout management RPC used over SSH by paired automation. It does not start
an HTTP service or install an agent. The former browser fleet dashboard is removed.

<div dir="rtl">

## خلاصهٔ فارسی

برای مدیریت هر سرور با SSH وارد شوید و `sudo backpack` را اجرا کنید. لینک تنظیم
سرور ایران را در سرور خارج وارد کنید یا پس از تست اتصال از تنظیم خودکار دو طرف
استفاده کنید. داشبورد مدیریت تحت وب حذف شده است.

</div>

Last verified against Backpack v1.18.0 (2026-10-27).
