# Automatic setup

*Last verified against Backpack v1.12.0, 2026-10-18.*

<div dir="rtl">

خلاصهٔ فارسی: در سمت ایران، Automatic کنار Reverse و Direct قرار دارد. فقط پورت‌های انتقالی را وارد کنید؛ برنامه آدرس عمومی، پورت‌های آزاد، رمزها، گواهی و مسیرهای سازگار را آماده می‌کند. در سمت خارج، Setup Kharej → Setup Link را انتخاب و لینک ایران را وارد کنید. نوع تونل و پروتکل‌ها از لینک خوانده می‌شوند. پورت‌های ورودی لازم پیش از ساخت نمایش داده می‌شوند؛ فایروال به‌صورت خودکار تغییر نمی‌کند.

</div>

On Iran choose **Automatic** and enter forwards such as `443,8080=127.0.0.1:2096`.
The public IPv4 address is detected; an address is requested only if detection fails.
Review the public TCP/UDP listeners and confirm creation. All other settings use
generated credentials, verified private certificate trust and the Balance preset.
No certificate private key is exported. Keep the setup link private: it contains
the paired account secrets.

A public domain is not required. For an IP endpoint, XHTTP gets its own generated
private DNS certificate identity and public trust in the link while continuing
to dial the actual IP. Other carriers retain certificates covering the IP; TLS
verification stays enabled on every path.

On Kharej choose **Setup Link** directly. Direction and protocol are inferred for
Reverse, Automatic, Direct and AmneziaWG links. Local certificate/key paths needed
by a Direct listener still belong to that machine and must be provided there.
Manual setup and the detailed managed-path editor remain available.

Automatic supports all eight compatible managed carriers: Short HTTPS, Naive H2,
XHTTP, REALITY, Hysteria2 with Salamander, AnyTLS, TUIC and ShadowTLS v3 with SS2022.
Only installed pinned helpers are included. REALITY/ShadowTLS require a successful
authenticated cover probe; unavailable carriers are explained and omitted.
At least two compatible carriers are required. Install the matching fork build
with `BP_HELPERS=naive,xray` on both machines to make all helpers available.
The Naive helper selection also installs the pinned sing-box runtime.

Native TCP, WS, KCP, packet carriers and Direct/L3/AmneziaWG are independent engines,
not compatible managed stream wrappers; they remain in their existing setup
flows. Automatic does not pretend to switch a live connection between these engines.

Each compatible path gets a separate free public port, avoiding requested forward
ranges and existing listeners. Port reservations are held through confirmation
and released immediately before service startup. A competing process may still
win a bind at startup; review the service state in that case. DNS/NAT/firewall
configuration stays with the operator; the summary names every required listener.

The adaptive engine verifies payloads before selecting a path. New connections
can fail over; active TCP streams stay on their original connection. Hysteria2
and TUIC enable native UDP; UDP and TCP use separate verified path selection.
When sing-box is absent the summary explicitly says TCP only. Packet loss during
an outage is not converted into reliable stream transport.
