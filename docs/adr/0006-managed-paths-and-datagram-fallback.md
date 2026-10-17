# Managed paths and datagram fallback

Status: implementation in progress.

The next transport work has five acceptance boundaries:

1. Managed reverse paths keep their listeners alive independently. A stable
   frontend chooses a path using authenticated, verified end-to-end transfers,
   with failure thresholds, a minimum dwell and a meaningful improvement margin.
   Existing streams remain pinned; switching applies to new connections.
2. Opt-in native reverse UDP for Hysteria2/TUIC uses the helper's UDP relay,
   bounded associations and authenticated flow identifiers. There is no stream
   fallback, unbounded outage queue or arbitrary unauthenticated destination.
3. MASQUE HTTP/2 uses RFC 9298 Extended CONNECT and RFC 9297 Capsules, verified
   TLS and a fixed destination. It is an explicitly selected TCP fallback:
   unlike HTTP/3 DATAGRAM it inherits reliable transport and head-of-line delay.
4. Short HTTPS gains managed lifecycle, pairing, trust renewal and real payload
   testing while retaining its bounded request and replay/offset contract.
5. MPTCP is opt-in for native TCP tunnel sockets, preserves ordinary TCP
   fallback and exposes the negotiated result. It does not change host routing,
   sysctls or the endpoint manager, and does not imply multiple physical paths.

Old configurations and their wire formats remain unchanged. New paired modes
require matching versions. Tests must exercise transfer, malformed credentials,
bounded resources, cancellation and recovery at public seams. Linux-specific
network namespace checks run on disposable CI runners.
