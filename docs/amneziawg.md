# Experimental AmneziaWG

## Managed setup

Use **BackPack v1.10.0 or later on both machines**; an older
binary cannot apply the new link. **Setup Iran → AmneziaWG** creates
an independent pair, stores Iran's private TOML and installs a per-tunnel
systemd service. **Setup Kharej → AmneziaWG** accepts the Setup Link, as does
`backpack link apply 'backpack://…'`.

The creator retains the generated peer identity in its private `peer_setup_key`
so Manage → Setup Link can reproduce the peer configuration, just as `awg init`
retains client.json. Only the receiving identity is exported; the creator's
own private key never travels in that link. Treat both configs and links as
private credentials, mode `0600`.

The `[awg]` engine is mutually exclusive with the other engines. It preserves
the exclusive TUN and peer-only route policy below. It does not install NAT,
default routes, forwarding sysctls or firewall rules. The normal CLI and panel
can start, stop, restart, delete and monitor it, and edit forwarded TCP/UDP ports
and forwarding limits. The creator's Iran side listens and Kharej dials it.
Backends must listen on their tunnel IP or wildcard, since only the peer host
prefix is routed. Pair creation uses the terminal.

Metrics contain the public endpoint, byte counters and state derived from a
recent handshake, never raw device IPC or keys. Creating the interface alone
does not mark it connected. Device failures retry under the tunnel context;
systemd restarts a failed process. Normal config reload/rollback management
applies. Pairing, endpoint, MTU and obfuscation edits require matching peer
configuration. The standalone JSON commands below remain available.

`backpack awg` is an opt-in **independent point-to-point L3 engine**, pinned to
[amneziawg-go v3.1.20260828](https://github.com/amnezia-vpn/amneziawg-go/tree/v3.1.20260828).
It carries IP using AmneziaWG's own authenticated protocol, rather than wrapping
the existing GRE/Noise tunnel in another encrypted tunnel. No helper download,
kernel module or `awg-quick` installation is needed.

Run on Linux with `/dev/net/tun`, `CAP_NET_ADMIN` (normally root), and `iproute2`.
UDP must pass between the hosts. Obfuscation cannot make a blocked IP or a
blocked UDP path reachable. The settings are a conservative bounded prototype,
not a claim that any particular ISP will carry the traffic.

## Create a pair

On the listening server, choose an unused UDP port and unused tunnel addresses:

```sh
umask 077
backpack awg init --endpoint 203.0.113.10:25999 --dir /root/awg-pair \
  --address 10.254.248.1/32 --peer-address 10.254.248.2/32
backpack awg check --config /root/awg-pair/server.json
backpack awg run --config /root/awg-pair/server.json
```

Replace the documentation IP with the listener's real address. `init` refuses
an existing directory. It generates two independent private identities, paired
public keys, a shared preshared key, a shared header-protection key and disjoint
randomized header ranges. **Copy only `client.json`** to the connecting host
over a trusted channel; `server.json` stays on the listener. Protect both files
with mode `0600`. Do not publish either file or paste it into an issue.

On the connecting host:

```sh
chmod 600 /root/client.json
backpack awg check --config /root/client.json
backpack awg run --config /root/client.json
```

Run the two processes in separate terminals, then verify the tunnel:

```sh
ping -c 10 10.254.248.1
```

The generated name is `bpawg0` on each machine. Change `interface` to another
`bpawg`-prefixed name for multiple pairs. An existing interface is refused
atomically: the command never attaches to or deletes another process's TUN.
Stopping with SIGTERM/Ctrl+C closes the device and removes its interface and
interface-bound routes. A failed startup also closes the new interface.

IPv6 tunnel addresses must be `/128`; both sides must use the same inner family.
The outer endpoint may be IPv4 or bracketed IPv6, for example `[2001:db8::10]:25999`.
The endpoint is a literal IP, not a DNS name, and must differ from the inner
addresses. The initial MTU is 1280, with validated padding bounded to fit a
1500-byte outer path. Paths with a smaller MTU require separate evaluation.

## What the prototype configures

Only its new TUN, its local address, one **peer host route**, and its own UDP
sockets. `AllowedIPs` contains exactly that peer host prefix. It never replaces
an existing route or changes default routing, NAT, IP forwarding, DNS, firewall
rules, global kernel tuning or existing BackPack tunnels. No UAPI control socket
is exposed, and private key material is never printed.

The command stays in the foreground. Interface readiness is not a handshake or
availability verdict: test actual payload transfer in both directions. Normal
WireGuard rekey behavior applies; after a peer loses its session state, recovery
may wait for the next handshake. Existing application connections may time out.

The standalone JSON command does not install services or use `[l3]`
configuration. Use managed setup above for menus, links, forwarding, services
and metrics. General routed subnets and endpoint failover remain outside this
bounded peer-only engine.
Use `awg check` before `awg run`; it validates configuration without creating an
interface, opening network sockets or modifying routes. `init` and `check` also
work on platforms where running the engine is unsupported.

## Verification

The test suite checks paired identities, invalid settings, file permissions,
encrypted IPv4/IPv6 packets over real loopback UDP, rejected source addresses and
rejected preshared keys. CI additionally uses real Linux TUN devices in isolated
network/mount namespaces, byte-identical transfers in both directions, duplicate
interface refusal, peer restart, a lossy path and cleanup after shutdown. These
tests establish implementation behavior, not resistance to every censor.

<div dir="rtl">

## خلاصهٔ فارسی

این موتور آزمایشی مستقل است و از رمزنگاری خود AmneziaWG استفاده می‌کند. با
`backpack awg init` دو فایل خصوصی ساخته می‌شود؛ فقط `client.json` را به سمت
اتصال‌گیرنده منتقل کن. هر دو سمت را با `awg check` بررسی و سپس با `awg run`
اجرا کن. لینوکس، TUN، دسترسی مدیریت شبکه و UDP قابل استفاده لازم است.
برای نصب مدیریت‌شده از منوی AmneziaWG استفاده کن؛ لینک همتا، سرویس دائمی،
فوروارد TCP/UDP و آمار اتصال به مدیریت معمول بک‌پک متصل‌اند. فقط رابط اختصاصی
و مسیر تک‌آدرس همتا ساخته می‌شوند. موفقیت را با انتقال واقعی داده بسنج؛
بالا آمدن رابط به معنی برقراری تونل نیست.

</div>

---

*Last verified against Backpack v1.12.0.*
