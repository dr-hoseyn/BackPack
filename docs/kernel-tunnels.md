# Standard kernel L3 and L2 tunnels

Choose **Setup Iran → Standard Kernel L3 / L2**. Select the method, literal
underlay addresses and tunnel IP pair. Apply its `backpack://` setup link using
**Setup Kharej → Setup Link**, or `backpack link apply '<link>'`. Use the same
build on both machines, v1.19.0 or newer. Install the method's host prerequisites
before applying its setup link.

Each tunnel is an independent `[kernel]` configuration and an ordinary Backpack
systemd service. Manage supports start, stop, restart, logs, setup links, edits
to MTU/carrier port/forwarded ports, and deletion. A different engine cannot
share the same configuration. Existing reverse, L3 and AmneziaWG tunnels keep
their current wire format and lifecycle.

## Methods

| Family | Methods | Requirements |
|---|---|---|
| IPv4 underlay, L3 | `gre`, `ipip`, `sit`, `6to4`, `6to4-gre` | Linux IP tunnel modules; `sit` carries IPv6 only |
| IPv6 underlay, L3 | `ip6gre`, `ipip6`, `ip6ip6` | Working IPv6 underlay; `ipip6` carries IPv4, `ip6ip6` IPv6 |
| Native encryption | `wireguard`, `xfrm-ipsec` | Kernel WireGuard plus `wg`, or strongSwan IKEv2/VICI with kernel-netlink |
| UDP encapsulation | `fou-gre`, `gue-ipip` | Linux FOU/GUE support |
| Parallel kernel routes | `ecmp-lanes` | 2–8 keyed GRE interfaces; default four |
| Ethernet, L2 | `eoip`, `gretap`, `ip6gretap`, `vxlan`, `geneve`, `l2tpv3`, `etherip` | Relevant kernel modules; EoIP/EtherIP additionally use a TAP device and raw socket |

`ipip`, `gue-ipip`, `ipip6` and `6to4` carry IPv4 payloads only;
`sit` and `ip6ip6` carry IPv6 payloads only. Configuration rejects incompatible
inner address families before touching network state.

All methods need Linux, `iproute2`, and root/CAP_NET_ADMIN. Raw bridges also
need CAP_NET_RAW and `/dev/net/tun`; reverse carriers need `/dev/net/tun`.
Kernel support varies by host. Unsupported modules produce a startup error,
and the service retries after five seconds. Dependencies are not downloaded
or installed implicitly. There is no MikroTik command generator.

Bare GRE/IPIP/SIT and L2 methods are unencrypted. The token derives paired
identifiers, but does not add encryption to a native unencrypted protocol.
Use WireGuard, IPsec or the authenticated reverse carrier when encryption is
required. Protocol availability and CPU/throughput must be measured on the
actual deployed route; adding a kernel method alone does not prove a speed gain.

## Native pair example

Iran:

```toml
[kernel]
method = "gre"
role = "iran"
local_addr = "192.0.2.1"
remote_addr = "192.0.2.2"
local_ip = "10.77.1.1/30"
peer_ip = "10.77.1.2"
interface = "bpk-example"
token = "REPLACE_WITH_THE_SAME_RANDOM_SECRET_OF_48_CHARACTERS"
mtu = 1380
ports = ["443", "2053=8443"]
accept_udp = true
```

Kharej uses the same method/token/MTU, `role = "kharej"`, swaps the underlay
addresses and tunnel addresses (`local_ip = "10.77.1.2/30"`,
`peer_ip = "10.77.1.1"`), and omits `ports` and `accept_udp`. The setup link
performs this mirroring, including the shared ID, port and GRE lane count.

Bare forwarded targets reach the Kharej **tunnel IP**, so the backend service
must listen there or on a wildcard address. Explicit targets can name another
routable address. Forwarded TCP/UDP uses Backpack's existing port forwarder
and limits. Traffic routed directly across the interface stays in the kernel
for the native kernel methods; EoIP/EtherIP and reverse use a userspace carrier.

The engine installs only the tunnel's own interface addresses and peer host
route. It does not replace the host default route, flush firewall rules or
enable global IP forwarding. Configure additional routes, bridges and forwarding
policy explicitly if using these tunnels to join larger networks. L2 methods
create an Ethernet interface; they do not automatically bridge a host's LAN.

## Reverse carrier

Set `reverse = true` on both endpoints, and choose a free `port` (default 24443
for the TCP carrier). Kharej initiates eight persistent TCP connections to Iran.
Each connection performs a mutually authenticated, ephemeral Noise handshake;
the authenticated registration binds the method, shared ID and MTU.

The inner IP flow determines a stable lane. Each lane has its own bounded
queue and writer, so congestion on one lane does not block the others. Lost
lanes reconnect; active connections are not migrated without interruption.
Reliable TCP encapsulation can introduce head-of-line blocking on lossy paths.
This carrier is Backpack-specific and does not claim to be TLS or FRP.

With `wireguard`, reverse instead makes Kharej the native UDP initiator.
Both endpoints retain ordinary WireGuard encryption and roaming behavior;
there is no TCP wrapper around WireGuard.

## WireGuard and IPsec

WireGuard keys and its additional preshared key are derived independently from
the random pair secret. Configuration goes to `wg syncconf` through stdin;
private keys do not appear in process arguments. The engine installs a peer
host route, not a default route.

IPsec requires a running strongSwan `charon` daemon with the default local VICI
socket, IKEv2 PSK, Curve25519, AES-GCM and kernel-netlink support. On Debian or
Ubuntu, install the corresponding strongSwan daemon, VICI/swanctl and crypto
plugins and enable the daemon before creating the tunnel. Native IPsec also
needs IKE UDP 500/4500 and ESP permitted on the underlay.

IKEv2 negotiates fresh traffic keys and rekeys the kernel ESP SAs. GRE stays
down until its protecting trap policy is installed, and goes down before the
policy is removed. Only the connection and credential named for this tunnel
are loaded/removed through VICI. Reserve the underlay address pair for IPsec;
its GRE traffic selectors must not capture another native GRE tunnel.

## Ownership, health and tests

An interface name is exclusive. The process holds a namespace-specific lock;
it refuses an existing interface without its Backpack ownership alias. Startup
failures unwind completed operations in reverse order. Shutdown joins device
readers, sockets, queues and forwarders before deleting owned interfaces. A
crash can leave FOU receive ports or L2TP objects behind; inspect those specific
objects before retrying rather than deleting unrelated host network state.

An interface being configured does not prove peer reachability. Management
probes the peer through the selected interface and briefly caches that result
for the exact credential generation. A successful probe reports online; a
missing interface reports offline, while a blocked/unavailable ping remains
unknown and does not trigger a restart by itself. Health Check shows this
distinction explicitly. The
Connection Test now includes all 20 methods in native and reverse modes.
These cases run serially after the existing carriers, using fresh temporary
interfaces and peer routes. The worker acknowledges cleanup before the next
case; failed cleanup stops the remaining kernel phase. Missing privileges,
modules, tools, VICI, incompatible underlay families or occupied subnets are
reported as skipped, rather than evidence of provider filtering.

Quick observes each kernel case for two seconds, then measures verified
four-stream download and upload with 100 ms warmup and 500–750 ms sampling
per direction. Silent native paths get 3 seconds, reverse carriers 5 seconds,
and WireGuard/IPsec 8 seconds for their handshake/retry. Full retains its
60-second observation, discovery allowance and sustained speed profile.
Control generations are checked every 100 ms; the result table remains limited
to twice per second. All 40 method/direction candidates remain in the suite.
This short Quick sample cannot reveal a flow cut after 17 seconds: use Full
for persistent-path validation. Total time includes each reachable method's
setup, observation, speed and teardown; a complete suite is not a 20-second run.

Successful kernel results appear in recommendations, history and **Set Up A
Tested Tunnel**. Promotion keeps method, direction, port, subnet and tested MTU,
rotates token/identifiers and creates ordinary managed services. Bare targets
reach the peer tunnel IP. IPv6-only SIT/IP6IP6 produce routed interfaces;
leave forwarded ports blank. Setup verifies peer ping traffic before success.
Both endpoints need the updated build; an older worker skips kernel cases. The kernel-tunnel
CI matrix starts actual engines in disposable Linux namespaces and checks
bidirectional packets, MTU-sized payloads and teardown for each supported method.

<div dir="rtl">

## خلاصهٔ فارسی

از Setup Iran گزینهٔ Standard Kernel L3 / L2 را انتخاب کن و لینک را روی خارج
در Setup Link وارد کن. تونل‌های استاندارد کرنل، لایهٔ دو، وایرگارد، IPsec و
ECMP اضافه شده‌اند. حالت Reverse برای روش‌های معمول از هشت اتصال TCP با Noise
استفاده می‌کند؛ وایرگارد همچنان UDP بومی دارد. روش‌های خام رمزنگاری ندارند.

ابزارها و ماژول‌های لازم باید روی لینوکس موجود باشند. IPsec به strongSwan با
VICI نیاز دارد. وضعیت اینترفیس به‌تنهایی اتصال را ثابت نمی‌کند؛ عبور ترافیک را
از هر دو سمت بررسی کن. هر ۲۰ روش، مستقیم و معکوس، در Connection Test هستند؛ نتیجهٔ موفق را می‌توان
به تونل دائمی تبدیل کرد. Quick برای هر روش کرنل ۲ ثانیه بررسی و سپس سرعت‌سنجی
دارد؛ Full بررسی طولانی را نگه می‌دارد. تاریخچه، پیشنهاد و مدیریت سرویس هم
پشتیبانی می‌شوند. مسیر پیش‌فرض، فایروال عمومی و اتصال خودکار به bridge تغییر نمی‌کنند.

</div>

---
[← Back to the docs index](README.md)

*Last verified against Backpack v1.20.0*
