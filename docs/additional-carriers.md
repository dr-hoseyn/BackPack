# TLS, gRPC, DNS and SSH carriers

Update both Backpack peers before selecting these transports. These are new
Backpack carriers, not wire-compatible clients for Midos Tunnel.

The CLI offers them under **Setup Iran / Setup Kharej → Reverse**. TLS and gRPC use the existing TCP
reverse engine, forwarding, connection pool, limits and PROXY protocol support.
Their contents are authenticated and encrypted with Noise inside the carrier;
the shared tunnel token is never exposed to a TLS-terminating CDN.

## TLS and gRPC

Set `transport = "tls"` for a TLS byte stream or `transport = "grpc"` for a
bidirectional gRPC call over HTTP/2 and TLS. gRPC uses `/<grpc_service>/Tun`;
both peers must use the same service name (default `Backpack`). A CDN must
support streaming gRPC, HTTP/2 and the configured origin port.

Optional client keys:

```toml
server_name = "origin.example.com"
fingerprint = "chrome"
tls_fragment = false
grpc_service = "Backpack"
```

`fingerprint` accepts `chrome`, `firefox`, `safari`, `edge` and `random`.
`random` chooses one of these browser profiles for each connection. These
options apply to the new TLS/gRPC carriers. Existing WSS keeps its Chrome
profile. Fragmentation splits the first write; TCP may coalesce it, so this
does not guarantee packet boundaries or filtering bypass.

The server uses the existing `tls_cert` / `tls_key` or `acme_domain` certificate
settings, or generates a certificate when neither is set. `simple_auth` belongs
to WSS and is not needed for these carriers.

## DNS-shaped KCP

Set `transport = "dns"` on both ends. This is the existing encrypted KCP/FEC
and SMUX engine using direct UDP packets with DNS TXT framing. It supports the
same KCP tuning and forwards both TCP and optional forwarded UDP. Effective KCP
MTU is capped at 1100 bytes to leave room for the DNS envelope.

This is **not a recursive DNS tunnel**: it cannot route through a public DNS
resolver. The destination server's configured UDP port must be reachable.
Port 53 is optional and must not collide with a DNS service already running.

## SSH

Iran's existing sshd forwards each carrier to Backpack's private relay:

```toml
[server]
bind_addr = "127.0.0.1:62110"
transport = "ssh"
token = "SAME_LONG_RANDOM_TUNNEL_TOKEN"
ports = ["443=127.0.0.1:2096"]
```

On kharej:

```toml
[client]
remote_addr = "IRAN_IP:62110"
transport = "ssh"
token = "SAME_LONG_RANDOM_TUNNEL_TOKEN"
ssh_port = 22
ssh_user = "root"
ssh_key = "/root/.ssh/id_ed25519"
ssh_host_key = "SHA256:REPLACE_WITH_THE_IRAN_SSH_HOST_KEY_FINGERPRINT"
```

Obtain the host fingerprint on Iran, for example with
`ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub -E sha256`.
That must be the host key sshd actually presents. The client refuses an absent
or different fingerprint before authenticating. sshd must allow TCP forwarding.
`ssh_password` can replace `ssh_key`; the CLI supports either. An optional `ssh_remote` overrides the relay
destination, normally `127.0.0.1:<remote_addr port>`.

Setup links preserve public carrier settings, including the host fingerprint,
gRPC service and browser options. They omit the SSH password and private-key
path; provide those on the dialing host. An ordinary tunnel edit preserves the
carrier's settings and local credentials.

Connection Test exercises SSH with a temporary forwarding-only SSH endpoint.
It cannot execute commands or forward to arbitrary destinations. Production
SSH setup still needs the operator's real sshd login and host key, so the
temporary SSH test endpoint is not offered for automatic permanent deployment.

<div dir="rtl">

## خلاصهٔ فارسی

TLS و gRPC با اثرانگشت مرورگر، SSH با کلید میزبان پین‌شده و KCP با ظاهر DNS به تانل معکوس اضافه شدند. DNS به UDP مستقیم نیاز دارد و از resolver عمومی عبور نمی‌کند. رمز و مسیر کلید خصوصی SSH داخل لینک تنظیمات ارسال نمی‌شوند؛ آن‌ها را روی کلاینت وارد کن. برای همهٔ این حالت‌ها هر دو سرور را ارتقا بده.

</div>

*Last verified against Backpack v1.20.0, 2026-11-01.*
