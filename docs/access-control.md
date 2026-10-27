# Access control

This build exposes no management HTTP listener, browser login, API token endpoint
or browser terminal. Administer a host through its SSH login and `sudo backpack`.
Root access is required to manage systemd units, raw transports and TUN devices.
Use the host's SSH keys and sudo policy to control access; protect the configuration
and backup files as credentials because tunnel tokens and peer keys live there.

The optional Telegram bot retains its owner/admin permissions and private-chat
checks. Read-only admins can view readings; write actions require the appropriate
permission. There is no panel-password command or browser credential to disclose.

<div dir="rtl">

## خلاصهٔ فارسی

مدیریت این نسخه فقط از طریق SSH و CLI است. دسترسی به کانفیگ و بکاپ را مانند رمز
محافظت کنید. مجوزهای مالک و مدیران ربات تلگرام برقرار است؛ هیچ پنل یا رمز مرورگری وجود ندارد.

</div>

Last verified against Backpack v1.17.0 (2026-10-26).
