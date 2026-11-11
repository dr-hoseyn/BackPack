# WSSMux carrier scheduling

WS Mux and WSS Mux now assign a waiting user to an available carrier using
socket congestion first, active relay count second, and least-recent use to
break ties. Previously every session competed for the same wake-up channel,
so a quiet-looking session with a backed-up TCP socket could take the next
user even when another physical session was draining normally.

On Linux the scheduler uses the existing TCP_INFO observer: queued bytes,
delivery rate, receiver window and retransmission state estimate the delay in
front of a new stream. Samples are cached for 50 ms per physical session;
there is no additional accounting on application reads or writes. TLS and the
deadline wrapper are unwrapped only to identify the observed TCP socket. All
traffic still passes through the original TLS/WebSocket connection.
Idle sessions are not polled. TCP Forward retains its existing 200 ms cache.

Other platforms, unavailable kernel counters and unsupported wrappers fall
back to occupancy-based selection. No carrier is discarded just for having a
high score. A recovered carrier becomes eligible again at the next sample.
When all carriers have pressure, new users use the least congested available
one. FIFO order, pairing timeouts, queue capacity, stream admission limits,
pool growth and safe retirement retain their existing behavior.

Every available WebSocket carrier participates. This policy deliberately does
not use TCP Forward's separate warm-pair preference or its 2 ms grace period.
TCP Forward, TCP Mux and KCP keep their previous scheduling policy.

This is a server-side scheduling change with no new wire format or setting.
It helps place **new** streams; an existing stream remains on its original
carrier. It cannot remove TCP head-of-line blocking within a carrier.

## Related approaches

- [smux](https://github.com/xtaci/smux) supplies fair traffic shaping and
  version-2 per-stream flow control *inside* one session. Carrier selection
  handles the separate decision of which physical session takes a new user.
- [Envoy least request](https://www.envoyproxy.io/docs/envoy/v1.37.7/intro/arch_overview/upstream/load_balancing/load_balancers)
  uses active requests when selecting an upstream. Our generic streams do not
  expose HTTP request timing, so the congestion signal comes from the carrier
  socket rather than inventing application RTT measurements.
- [frp](https://gofrp.org/en/docs/features/common/network/network/) supports
  connection reuse and QUIC. [wstunnel](https://github.com/erebe/wstunnel)
  also provides WebTransport for paths where UDP works. Backpack already has
  QUIC/WebTransport carriers; changing transport is an independent choice.

## Verification

`TestWSMuxQueueSpreadsNewUsersAcrossEveryAvailableCarrier` checks that spares
participate immediately. Additional regressions check a stalled carrier with
fewer streams than a healthy carrier, recovery without a restart, and FIFO
reassignment when the selected carrier leaves. Existing mux queue, framing,
deadline and retirement tests continue to run.

`BenchmarkWSMuxCarrierSelection` measures only scheduling, not network
throughput. Compare complete transfers and fresh-user p95/p99 latency under
the same offered load before drawing conclusions about a WAN route.
