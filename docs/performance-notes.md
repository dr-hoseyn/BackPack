# Performance notes

Measurements that closed a question, kept because the question comes back.

Each of these was an idea that sounded right. Two of them are not shipped, and
the reason they are not is a number rather than an opinion — which is the point
of writing them down: without this file the same idea is proposed again next
year and measured again from scratch.

## How these numbers were taken

Every figure below is from this repository's own tests, run on an otherwise idle
machine, and the second half of that sentence had to be learned. The netns
harnesses used to leave the engine processes they started running as orphans —
a day of matrix runs had left 324 of them holding six and a half gigabytes — and
measurements taken beside them were quietly wrong in both directions. The
harnesses reap what they start now (`tools/*test/*.sh`), and every number here
was re-taken afterwards.

If a figure here disagrees with one you measure, check what else is running
before concluding anything about the code.

## Where the time actually goes

Two profiles, taken 2026-09-22.

**A tunnel under sustained load** (the soak: 650,000 round trips through a real
tunnel over loopback):

```
59%  syscalls
 9%  runtime.nanotime
 4%  runtime.futex
```

**The layer-3 data path on its own** (seal, parse, open, at full MTU):

```
62%  chacha20poly1305Seal      (assembly)
16%  chacha20poly1305Open      (assembly)
 3%  session.sealKind          (Go)
 6%  allocation
```

So the product spends its time in the kernel and in hand-written crypto
assembly. That single fact decides most of what follows.

## Profile-guided optimisation: measured, not shipped

Go's PGO works by using a profile to guide inlining and devirtualisation of
**Go** code. Given the two profiles above there is very little Go code to guide:
the tunnel is syscalls, and the data path is assembly in `x/crypto` that the Go
compiler does not touch.

Measured anyway, with the data-path profile as `default.pgo`, three runs each:

| | without | with |
|---|---|---|
| seal, full MTU | 856 ns/op | 883 ns/op |
| full round trip | 1725 ns/op | 1690 ns/op |

Both differences are inside run-to-run variance, and they point in opposite
directions. There is no gain here to ship, and a committed `default.pgo` is a
file that has to be regenerated and re-justified on every release.

**Revisit if** the balance changes — a pure-Go crypto path on an architecture
without assembly, or a workload where the Go relay code dominates rather than
the kernel.

## splice: measured, not turned on by default

`splice` saves a copy between two sockets and the kernel.

| | throughput |
|---|---|
| buffered | 8,419 Mbit/s |
| splice | 10,813 Mbit/s |

+28%, on loopback, for plain `tcp` with no bandwidth limit — and loopback
*understates* it, because the copy splice avoids is cheap there and the syscall
overhead dominates. A gain here is conservative; a loss would have been damning.

It is still off by default, and that is the recorded decision rather than an
absence of one. A synthetic gain shows the implementation works, not that it
should become everybody's default on upgrade. What would justify flipping it is
the same measurement on a real link. Reproduce with:

```
go test ./internal/e2e/ -run TestSpliceThroughput -v -count=1
```

## recvmmsg: measured, shipped

40,000 datagrams of 1200 bytes at a loopback carrier:

| | received | rate |
|---|---|---|
| one at a time | 27,682 of 40,000 | 59 kpps |
| recvmmsg | 40,000 of 40,000 | 214 kpps |

The rate is the smaller half of that result. Reading one at a time, the receive
loop could not keep up with the sender and the socket dropped nearly a third of
the datagrams. A tunnel losing packets inside its own receive loop is
indistinguishable from a lossy path when you are looking at it from outside,
which is the most expensive kind of fault to chase.

Linux only, plain `udp` carrier only. Every other carrier reads as it did
before, including if the batch path refuses at runtime — losing the
optimisation costs throughput, never the tunnel.

## sendmmsg: measured, shipped, and the number is not the point

The transmit half of the same mechanism. 40,000 datagrams of 1200 bytes at a
loopback carrier, three runs each:

| | syscalls | rate (median of 3) |
|---|---|---|
| one at a time | 40,000 | 254 kpps |
| sendmmsg | 5,000 | 260 kpps |

**Eight times fewer syscalls for the same throughput.** Read that as the
expected shape rather than a disappointment: on loopback the syscall is not what
costs, the copy and the sink are, so removing seven eighths of the syscalls buys
nothing measurable here. It costs nothing either — and on a real NIC, where the
syscall and the driver path *are* the cost, the same change is what the receive
side measured as 59 → 214 kpps once that path was genuinely loaded.

It is shipped on those terms: same throughput, far less work, and the direction
of the remaining uncertainty is the one that favours it.

The send path was already reading a *batch* from the TUN — the kernel hands over
however many packets it had ready — and then writing each one with its own
syscall. The gather costs nothing to arrange: the packets are already there,
already sealed, already going to the same peer.

## UDP segmentation offload: measured, shipped, and the number *is* the point

`sendmmsg` removed seven eighths of the syscalls on the layer-3 send path and
bought no measurable throughput, which is the row above. The conclusion drawn
from it was the right one and it was only half the story: the cost is not the
syscall, it is **per datagram inside the kernel** — a copy into the socket
buffer and a walk down the protocol stack, once each, for every datagram.
Batching the calls does nothing about that.

`UDP_SEGMENT` does. One buffer and a segment size go in, the kernel cuts the
buffer up, and the copy and the stack walk happen once for the whole run.

40,000 datagrams of 1,200 bytes to a drained loopback sink, three runs, with an
unconnected socket and an explicit destination — the same shape the carrier has,
because a connected socket measures faster and would not be measuring the thing
that could ship:

| | syscalls | rate | vs sendmmsg |
|---|---|---|---|
| `sendmmsg`, 32 per call | 1,250 | ~265 kpps | — |
| `UDP_SEGMENT`, 32 per call | 1,250 | ~1,050 kpps | **+206% … +377%** |
| `UDP_SEGMENT`, 48 per call | 834 | ~1,170 kpps | +264% … +414% |

**The middle row is the one that decided it.** Same batch, same number of
syscalls, three to four times the rate — so the gain is the offload itself and
not the batch size.

The third row is not just a curiosity: the TUN hands the send pump whatever the
kernel had ready, which is far more than eight, so the carrier splits a batch
into as many segmented writes as it can rather than taking one and refusing the
rest. A run ends at the first packet of a different size, at the kernel's
segment ceiling, or at the edge of one UDP payload — and a *short* packet does
not merely end a run, it is that run's last segment, because the kernel's
remainder is the final datagram. So a batch of full-sized packets with an
acknowledgement in the middle of it goes out as two segmented writes rather than
as none.

Shipped with two conditions, both in `internal/tunnel/l3/gso.go`:

- **Every segment but the last must be the same size.** That is the mechanism,
  not the implementation. A run of full-sized packets — a transfer through the
  tunnel — qualifies; the ragged traffic in between does not and takes the
  `sendmmsg` path exactly as before. The optimisation therefore applies where
  the tunnel is busiest, which is the right place for it.
- **Support is found out by trying.** It depends on the kernel, the family and
  the route, and there is no reliable way to ask first. The first refusal turns
  it off for the life of the socket and the same batch goes out the old way,
  so nothing is dropped and nothing keeps paying for a probe that has failed.

`TestGSOSendRate` is the measurement and it is kept, so the next person to ask
gets a number from their own machine.

**Shipped, and until v1.8.5 not reached.** The default `udp` carrier was wrapped
in a peer-pinning layer that exposed neither batch method, so recvmmsg, sendmmsg
and `UDP_SEGMENT` above ran only in their own tests; every production packet was
its own system call. v1.8.5 hands a single path to the tunnel unwrapped
(`TestTheDefaultUDPCarrierReachesTheBatchPaths` holds it there), and a live tunnel between two
network namespaces shows the offload working: 5 MB through the tunnel left the
dialling engine in 168 `sendmsg` calls for about 4,000 datagrams — some 24
segments a call — and no `sendmmsg` fallback at all.

## How wide a receive batch, and how many readers

Two questions about the layer-3 receive path, asked together because the second
one only makes sense once the first is answered. `TestBatchWidth` and
`TestReadersPerSocket` are both kept.

**The batch was eight and eight was an argument, not a measurement.** Four
widths, three runs, 40,000 datagrams of 1,200 bytes:

```
width   1    ~50 kpps, and only half the datagrams arrive
width   8   ~225 kpps
width  32   ~300 kpps
width 128   ~275 kpps
```

It is now thirty-two, and thirty-two is the peak rather than a compromise: a
hundred and twenty-eight is no faster and sometimes slower, while costing 900 KB
more of buffers per tunnel.

The interesting part is not the rate. The batch rarely *fills* at any width —
two to four datagrams per call, because the reader keeps up — so the gain is not
in gathering more, it is in taking an occasional burst in one call rather than
two. And in the first row: a reader taking one datagram at a time cannot keep up
at all, and the socket drops nearly half of them.

**One reading goroutine is not the bottleneck any deployment will meet.** With
four senders pushing at once it takes 500–800 kpps and loses nothing. At
1,200-byte inner packets that is five to eight gigabits a second of tunnelled
traffic.

**And the mechanism usually proposed for raising it does not apply here.**
`SO_REUSEPORT` with N sockets distributes incoming datagrams across them **by
flow**, and a layer-3 tunnel has exactly one flow: one peer, one source port.
Every datagram hashes the same way, so N sockets would leave N-1 of them idle.
That is a property of the protocol, not of this implementation.

What is left is N goroutines reading the *same* socket, which the kernel allows
and serialises. Measured, three runs, one/two/four readers against the same
load: two and four are never faster than one and are usually slower. The
serialisation is the ceiling, and adding goroutines adds contention to reach it.

So the receive path stays one reader. The number to remember is the one that
makes it a non-question: a tunnel would have to be carrying several gigabits a
second before the reader was what limited it.

## Batching the reverse relay: there is nothing to gather

The layer-3 carrier gained `recvmmsg` and `sendmmsg`, and the obvious next
question was the reverse relay: it copies connection by connection and nothing
gathers. The answer is that the gather has already happened, in the kernel,
before the relay sees the data.

Measured on a saturated stream — 256 MB through a relay loop the same shape as
`handlers.transferData`, with the same 64 KB buffer:

```
4,501 reads for 256 MB
  full 64 KB buffer   3,942 reads   87.6%
  8–16 KB               428 reads    9.5%
  everything else       131 reads    2.9%
```

Nearly nine reads in ten already come back with the buffer full. There is no
batching to add: one read and one write per 64 KB is the floor for a stream, and
`splice` removes even the copy between them where both ends are plain TCP.

The remaining tenth is the interesting part, and it is the argument against
doing this rather than for it. Those are the reads that returned early because
that is all that had arrived. Gathering them means holding the first chunk until
a second one turns up — buying a syscall with latency, on a path that is already
the slow part of somebody's connection, for traffic that is small precisely
because it is interactive.

What does help at this layer has already been done and is measured above: a
64 KB buffer rather than 16 KB (a quarter of the syscalls per gigabyte), pooling
it rather than allocating per direction per connection, and `splice` where the
operator asks for it.

## Automatic MSS discovery on the reverse tunnel: not built, and why

The layer-3 engine measures its own path (`auto_mtu`). The reverse tunnel does
not, and the obvious symmetry — have every TCP connection read the path MTU the
kernel knows for its route, derive the clamp from it, and apply it — was built
and then thrown away, because it does nothing.

Measured in a network namespace with loopback narrowed to 1280 bytes, which is
the only way to get a genuinely narrow link into a unit test:

```
lo MTU = 1280
IP_MTU (what the kernel knows about the route) = 1280
the clamp that would be derived from it        = 1228
TCP_MAXSEG on the dialled socket, no clamp set = 1228
TCP_MAXSEG on the accepted socket, no clamp set = 1228
```

The clamp derived from the kernel's figure is the figure the kernel had already
chosen. That holds in every case the feature would cover:

- **The local interface is narrow.** The kernel sizes its advertised MSS from
  the outgoing interface and gets it right without help.
- **A remote hop is narrow and answers.** The ICMP "fragmentation needed" reply
  lowers the kernel's path MTU, and `snd_mss` with it. `IP_MTU` reports the
  result rather than causing it.
- **A remote hop is narrow and is silent.** This is the fault the clamp exists
  for, and it is exactly the case where `IP_MTU` still reads the local
  interface's number. A clamp derived from it would be 1448 on a path carrying
  1240 — no better than none.

What actually finds the number on a silent path is the kernel's own TCP MTU
probing, which searches downward when a connection stalls with retransmits and
no ICMP. `internal/optimize/optimize.go` has set `net.ipv4.tcp_mtu_probing = 1`
since before this was asked, so the discovery the feature would have added is
already running, at the only layer that can see the evidence it needs.

The part that is genuinely missing is neither of these: it is that the operator
is never *told* the kernel had to back off. Health Check reads `snd_mss` from
the live sockets and compares it to the path (`internal/manage/diagnose_path.go`
— `fromKernel`), which is the right place for that to appear.

## pck: a clock read nobody used, and batches at both ends

Profiled on 2026-09-25, a layer-3 `pck` tunnel between two network namespaces,
512 MB echoed and 16 parallel 64 MB downloads, three runs of each. `pprof`
of the loaded kharej side, before anything changed:

| Where | Share of CPU |
|---|---|
| syscalls — `sendto` on the packet socket, TUN write, packet-socket read | 54% |
| `time.Now`, almost all of it from `pckConn.peerFor` | 15% |
| ChaCha20-Poly1305, both directions | 6% |

The clock read stamped a `touched` time on every segment in both directions,
and nothing read it. On this machine's clock source a read is not the cheap
vDSO kind, which is also true of a good share of VPS hypervisors. It went, with
the per-packet `addr.String()` the peer map was keyed on:

| | one stream | CPU (Iran / kharej) |
|---|---|---|
| before | 970 Mbit/s | 18.3 / 16.5 s |
| without the clock read | 1,315 Mbit/s | 13.6 / 11.9 s |

That left the syscalls, one per segment each way. The receive side now reads
the packet socket with `recvmmsg` and hands the interface the whole batch in
one write; the device coalesces a flow's segments before the kernel's receive
path sees them. The send side uses `sendmmsg`. On one stream each is worth a
few percent — the section on sendmmsg above says why — but not on the
workload a tunnel actually carries:

| 16 parallel downloads | throughput | CPU (Iran / kharej) |
|---|---|---|
| v1.8.2 | 742 Mbit/s | 18.8 / 21.0 s |
| all three changes | **1,300 Mbit/s** | **7.5 / 11.9 s** |

The reverse `pck` transport runs under KCP, which only batches on a real UDP
socket, so it gains the clock fix alone: 545 → 631 Mbit/s on the same test.

The same batching on xdi's raw ICMP socket (x/net's `ipv4.PacketConn`, which
is recvmmsg and sendmmsg underneath) doubled it: 16 downloads 543 → 1,150
Mbit/s, 16 uploads 627 → 1,325, one stream 582 → 1,112. Sending had been 45%
of a loaded xdi tunnel's CPU. The upload figure also carries a second change:
the server's kernel answered every data-carrying Echo Request with a full-size
Echo Reply, which one iptables rule now drops (108,000 of them in a 64 MB
upload test).

One trap on the way, kept as a test (`tunstage_linux_test.go`): with offload
on, the TUN library coalesces by appending to the first packet's buffer in
place when its capacity allows. The write path staged packets back to back in
one buffer, so each had the rest of it as capacity and the append ran over the
next packet. Nothing had written more than one packet at a time before, so it
had never shown. Each packet now gets a 64 KB slot of its own.

## Forwarded UDP and quiet WebSocket resource use (2026-11-03)

Reviewed against `dr-hoseyn/BackPack` main at `b97a566c`. These changes reduce
work on the forwarding path without changing authentication, framing, configured
bandwidth limits or peer compatibility.

- Native UDP entry sockets now read `netip.AddrPort` values and look up flows
  using the forwarded socket and source endpoint together. Previously every
  packet allocated source-address storage and formatted both endpoints into a
  string. Replies and expiry use the same value address. Two forwarded ports
  reached by the same sender still have independent flows and backend choices.
- Native UDP decrypts fragments in the receive scratch. Reassembly consumes
  them before the next read; unfragmented queued messages retain independent
  storage, including empty datagrams. Client peer checks compare UDP endpoint
  fields instead of allocating formatted strings for every fragment.
- Raw reverse UDP server listeners use value address reads and materialize
  `UDPAddr` only for new associations. Mapped IPv4 sources are canonicalized to
  the existing string-key form so dual-stack reads and flow retirement agree.
- Raw reverse UDP uses `Read` on its connected tunnel/backend sockets, avoiding
  an unused source address on every datagram. Empty datagrams and packet
  boundaries retain their existing behavior. Read deadlines and server idle
  timers are refreshed on expiry rather than on every packet. Shared monotonic
  activity still gives either direction a full 60-second idle budget.
- WS/WSS forwarding starts its TCP read direction with the adaptive 16 KiB
  buffer and grows to 64 KiB after a full read. The receive direction borrows a
  bulk copy buffer only while copying a WebSocket message. A quiet connection
  therefore checks out 16 KiB of relay buffers instead of 128 KiB before bulk
  traffic. This is buffer ownership, not a claim about total process RSS:
  `sync.Pool` may retain returned buffers until a later collection.
- The WebSocket relay runs one copy in its caller, saving one goroutine per
  connection. A context cancellation callback closes both sockets; the caller
  still joins both copy directions before returning its connection quota.
- Disabled TCP-to-WebSocket trace logging no longer boxes a byte count on every
  write. An overwritten assignment in port mapping expansion was also removed
  so the full Linux staticcheck gate passes.

Allocation measurements below use Go 1.26.10, Windows/amd64, an Intel
Core i7-11370H, three 300 ms runs per case. Each before/after pair runs the same
new benchmark against the baseline/current production source. Native UDP uses
real forwarded socket reads and encryption, with a synchronous encrypted sink.
The authenticated-receive benchmark supplies valid cipher frames synchronously
and includes the source cipher's nonce costs; it excludes sockets and helper
processes. Raw UDP and WebSocket benchmarks use real loopback sockets.

| Path | Before: objects/op | After: objects/op |
|---|---:|---:|
| Native UDP entry, 64 B, IPv4/IPv6 | 10 | 1 |
| Native UDP entry, 1200 B, IPv4/IPv6 | 11 | 2 |
| Native UDP authenticated client receive, 64 B | 9 | 3 |
| Native UDP authenticated client receive, 1200 B | 20 | 6 |
| Native UDP authenticated client receive, 60000 B | 542 | 122 |
| Native UDP authenticated server receive, 60000 B | 182 | 122 |
| Raw connected UDP copy, 1200 B | 1 | 0 |
| WebSocket send, 1024 B / 64 KiB | 2 | 1 |

Native UDP allocated bytes fell from 160 to 16 B/op for 64-byte IPv4
payloads, and from 176 to 32 B/op for 1200-byte IPv4 payloads. IPv6 fell
from 144 to 16 and from 160 to 32 B/op respectively. The remaining entry
allocations are the cipher's per-fragment nonce. Authenticated client receive
of a 60000-byte datagram fell from about 132825 to 67540 B/op. Timing varied between runs;
these measurements establish lower allocation pressure, not a WAN throughput
increase.

Reproduce on Linux:

```sh
go test ./internal/nativeudp -count=1 -timeout 2m
go test -race ./internal/nativeudp ./internal/utils/handlers -count=1 -timeout 3m
go test -race ./internal/client/transport -run '^(TestRawUDP|TestUDPBackend)' -count=1 -timeout 2m
go test ./internal/nativeudp -run '^$' -bench '^BenchmarkNativeUDP(ForwardedEntry|AuthenticatedReceive)$' -benchmem
go test ./internal/client/transport -run '^$' -bench '^BenchmarkRawUDPConnectedCopy$' -benchmem
go test ./internal/utils/handlers -run '^$' -bench '^BenchmarkWebSocketRelaySend$' -benchmem
go test ./internal/server/transport -run '^TestQuietUDPDirection' -race -count=1
go test ./internal/server/transport -run '^$' -bench '^BenchmarkRawUDPTunnelReceive$' -benchmem
```

Validation in this session: native UDP's complete tests and race tests passed,
including encrypted IPv4/IPv6 replies, empty/fragmented payloads, two forwarded
ports sharing one sender, and an allocation regression gate. The allocation
gate fails on the baseline source and passes on the changes. Handler tests and
raw UDP client regressions passed normally/under race where selected; WebSocket
tests cover full duplex, peer close, idle cancellation, final bytes with EOF,
large/multiple messages and adaptive buffer growth. The new buffer-growth test
also fails on the baseline source. Receive ownership tests leave successive
messages queued while decrypting later frames; peer checks cover IPv4, mapped
IPv4, IPv6 scope zones and wrapper addresses. A fake-clock server test advances
through 111 seconds of shared idle activity without real waiting.

The Windows handler/client runs used a temporary Go overlay to adapt existing
Linux-only socket-option declarations in `reuse_port.go` and `tcp_dialer.go`.
The server's focused Windows runs additionally supplied an unused listener
declaration in that overlay; timer and UDP tests never call it. Those portability
substitutions are outside the patch. Full Linux `go build`,
`go vet` and `staticcheck` passed without that overlay. `govulncheck` reported
zero reachable vulnerabilities; it also listed three advisories in required
modules that the application does not appear to call.

The complete Linux end-to-end/soak suite was not executed on this Windows host.
Direct Windows execution also found existing platform failures in metrics
permission tests and the client's refused-dial/source-scanning tests; the client
failures reproduce against `b97a566c`. Linux CI now runs the complete test, race and coverage suites on the PR; its
latest status is recorded in the PR description. The preceding `71e296ed` run
passed full test/race and sustained-load suites; its instrumented timing failure
is addressed below. WAN capacity testing remains necessary before a deployment;
no release or server configuration was changed.

## WSSMux frame overhead and failed-session recovery (2026-11-03)

The mux deadline wrapper now remembers the effective write deadline. Repeated
frame writes with the same deadline leave the socket timer alone. Explicit
stream deadline changes always reach the socket; combined upgrade deadlines,
failed changes and connection close invalidate or update the cached value.
On the same Windows/amd64 host, three 300 ms real-socket microbenchmarks changed
`BenchmarkWSMuxFrameDeadlineRefresh` from 43.9–47.3 to 15.0–15.6 ns/op, with
zero allocations on both revisions. This measures deadline maintenance only,
not end-to-end WSSMux speed.

A fatal framed read/write now closes the physical connection immediately.
Gorilla cannot resume the stream after such an error. Previously an idle smux
reader could remain blocked after a writer failed, keeping a client pool slot
occupied until keepalive expiry. TLS close also needs to abort its underlying
socket first: uTLS otherwise attempts `close_notify` with a five-second write
budget on the congested socket. A real authenticated TLS regression test exposed
that extra wait. WS and WSS tests now check both the write deadline and the idle
reader's prompt exit, including five race-detector repetitions. The new idle
reader assertion fails against the original source.

WSSMux also now supervises fatal HTTP/TLS listener exits. A permanent accept
error previously closed the tunnel port while its generation continued waiting
for cancellation, so reconnecting clients could never reach a replacement
listener. The supervisor now closes pending HTTP sockets and requests a new
generation, using the existing listener-release barrier and bind retry policy.
The delayed recovery carries its original context and checks it under the shared
restart lock: an old listener's failure cannot restart a newer generation after
a reload. Intentional shutdown and certificate configuration errors retain their
existing behavior. Tests inject permanent accept failures for WS and authenticated
WSS, prove a replacement control channel authenticates, and cover shutdown and
stale-generation recovery under the race detector.

Backend work now also belongs to the physical mux session that accepted it,
not just the longer-lived control generation. Losing one data connection
cancels that session's pending TCP/UDP backend DNS/dials and idle flows while
sibling sessions remain usable. A regression holds backend DNS for a 30-second
dial budget, closes its data session, and verifies all work joins within one
second without cancelling the generation; a sibling session carries an echo
throughout. Both TCP and UDP forms fail against the prior source and pass under
the race detector with session-scoped cancellation. Normal TCP backend failures
still reach the existing health report; cancelled session dials no longer report
a healthy local service as unreachable.

The WSS Mux security/capacity workflow retains its existing authentication,
600 Mbit/s download, 1000-user mixed-load and quiet-session retirement checks.
A manual dispatch may additionally set `baseline_ref` to a git revision, building
the same public-configuration resource fixture for both revisions on one runner.
Three alternating samples per revision report CPU/GiB, goodput, fresh-user p99
and idle heap in the `wssmux-measurements` artifact. This comparison does not
replace or weaken any existing required check.

The explicit-baseline Linux run completed successfully:
[WSSMux verification and comparison](https://github.com/dr-hoseyn/BackPack/actions/runs/41621205212).
It compared `b97a566c` against production changes at `b5595a93`. The additional
receive-frame optimization below was measured separately against `b2a6ebf6`. Medians of three alternating
samples, with the same 1000 idle users and 600 Mbit/s offered workload:

| Metric | Baseline | Candidate |
|---|---:|---:|
| Verified application goodput | 599.4 Mbit/s | 599.7 Mbit/s |
| CPU seconds per GiB | 15.98 | 15.96 |
| Fresh-user p99 | 0.850 ms | 0.991 ms |
| Idle heap delta | 107920040 B | 107811320 B |

These differences do not establish a throughput, CPU or memory improvement for
WSSMux as a whole. The lower deadline-maintenance overhead is measurable in
isolation, and failed-session recovery is covered by regressions on both the
client/uTLS and server/standard-TLS directions. The same run passed negotiated
framing/authentication, the 600 Mbit/s capacity gate (7070.3 Mbit/s download on
that runner's loopback), mixed-load verification and quiet-session retirement.
Loopback capacity does not predict capacity on a real Internet route.

The SOCKS packet adapter used by native Hysteria2/TUIC also reads value source
addresses. Its real-loopback receive benchmark changed from one allocation to
zero per packet. Source validation, SOCKS fragmentation rejection and
empty/large datagrams are covered independently of optional helper binaries.

## Receive-frame reuse: measured and subsequently withdrawn (2026-11-03)

An initial smux-to-TCP optimization used smux's `WriterTo` to consume each owned
receive frame directly, without retaining a second relay buffer or copying
each frame through it. The experiment unwrapped only the known accounting
wrapper; pacing/unknown wrappers and destinations other than real TCP sockets
kept the existing buffered path. TCP consumes the borrowed slice synchronously,
so smux can recycle it after Write returns. Failed or short writes end the
stream instead of allowing a frame tail to be discarded. Tunnel byte counts,
EOF, cancellation and live throughput counting are covered for smux versions 1
and 2. Legacy sniffer settings no longer produce per-port usage files.

The change was first measured outside the PR branch. The
[bounded-profile comparison](https://github.com/dr-hoseyn/BackPack/actions/runs/41632246215)
used three alternating processes per revision, 1000 idle users and 600 Mbit/s
offered load; the
[resource and lossy-path comparison](https://github.com/dr-hoseyn/BackPack/actions/runs/41632932057)
used five alternating loopback samples and three per lossy-path protocol mode.
Both use the same public-configuration fixture against baseline `b2a6ebf6`.

| Workload / metric | Baseline | Receive-frame reuse |
|---|---:|---:|
| Bounded: 1000-user idle heap delta | 107512088 B | 74989400 B |
| Bounded: application goodput | 599.5 Mbit/s | 599.5 Mbit/s |
| Bounded: CPU seconds/GiB | 11.10 | 10.96 |
| Default resource profile: idle heap delta | 115222776 B | 82789992 B |
| Default resource profile: goodput | 6380.7 Mbit/s | 6421.8 Mbit/s |
| Lossy path, auto version 1: goodput | 77.7 Mbit/s | 82.2 Mbit/s |
| Lossy path, explicit version 2: goodput | 84.5 Mbit/s | 78.5 Mbit/s |
| Lossy path, auto version 1: fresh-user p99 | 1294.138 ms | 1071.344 ms |
| Lossy path, explicit version 2: fresh-user p99 | 1489.661 ms | 1302.468 ms |

The initial result was 28–30% lower idle heap, about 32 KiB less retained
relay storage per forwarded connection across both endpoints. These are combined
client/server heap figures, not a promise about process RSS on every server.
CPU changes were small. The explicit-version-2 lossy sample was about 7% slower,
while auto version 1 was faster; all existing CPU, goodput and fresh-user latency
regression gates passed. This evidence supports a memory improvement within
those performance tolerances, not a general increase in Internet throughput.
Existing stream windows, mux versions, session limits and bandwidth caps were
unchanged. The quiet-user retirement and authenticated capacity checks passed.
The direct-frame optimization was later withdrawn as described below; these
memory reductions are historical measurements, not a benefit of the final code.

Reproduce focused tests and the socket benchmark on Linux:

```sh
go test -race ./internal/utils/handlers -run '^TestMuxReceive' -count=3 -timeout 2m
go test ./internal/utils/handlers -run '^$' -bench '^BenchmarkMuxToTCPRelay$' -benchmem
```

The resource workflow's optional `baseline_ref` input now permits the same
CPU/goodput/latency/heap gates against an explicit revision; PR runs retain
`v1.16.0` as their baseline and keep every previous threshold. Artifacts record
both baseline and candidate commit hashes.

### Restoring read-side credit before blocked backend writes

The follow-up investigation found a reproducible flow-control difference in
smux v2: `Read` advertises the configured receive window before the backend
write, while `WriterTo` waits for that write to return. With a congested TCP
backend, the latter can strand the sender at its initial 256 KiB window even
though the session still has receive capacity. A real-socket
[Linux regression](https://github.com/dr-hoseyn/BackPack/actions/runs/41644508025)
fails on the previous `WriterTo` source and passes three times under the race
detector on the corrected `Read` path. It also checks cancellation joins both
relay workers.

A smaller-buffer `Read` alternative fixed that stall, but it did not meet the
stricter throughput target in a separate
[five-seed, longer lossy-path comparison](https://github.com/dr-hoseyn/BackPack/actions/runs/41643905868).
Each independent process transferred 256 MiB of verified application data with
eight workers, 80 ms RTT, 0.1% loss and a 200 Mbit/s link. Baseline buffered reads,
direct-frame forwarding and 1 KiB initial reads measured median goodputs of
86.2, 83.6 and 82.3 Mbit/s respectively. The 1 KiB candidate missed the
predeclared target of at least 98% of baseline throughput and was not retained.
This also shows why the earlier three-sample 7% difference is not an exact
estimate of a universal slowdown.

The final relay restores the original pooled `Read` path: 16 KiB initially,
growing to 64 KiB after a full read. All production Go source matches recovery
revision `b2a6ebf6`; the listener/session fixes and deadline amortization remain.
The 28–30% heap saving from direct-frame reuse is deliberately withdrawn in
favor of the established forwarding behavior. Network packet loss itself is
unchanged; this addresses application flow control and the risky optimization.

The optional `wssmux-loss.yml` workflow rotates the buffered reference, prior
direct-frame reference and candidate across five paired seeds, recording every
sample and revision. Its stricter 98% throughput diagnostic is separate from
the unchanged required resource/quality gates. The final rollback compares
memory with the buffered baseline; it does not claim the abandoned heap saving.

## What is measured on every build

The numbers above are one-offs. These are gates, and they fail the build:

- **Allocation counts** on the packet path and the relay buffer pool. Exact, not
  a maximum: deterministic, unaffected by the machine, and every one of those
  paths is meant to be allocation-free or close to it.
- **A wall-clock ceiling** at roughly 3× what each path measured when the
  ceiling was set. Generous on purpose — a shared CI runner varies by more than
  any tight threshold — and it still catches the failure that actually happens,
  which is a syscall or an allocation appearing in a per-packet loop. That is an
  order of magnitude, not a margin.

The combined benchmark gates skip under `-race`, which multiplies wall-clock
time by between five and twenty; dedicated allocation tests still run. Under
coverage, the benchmark allocation assertions remain active while wall-clock
ceilings are enforced by the ordinary, uninstrumented test suite. Instrumented
coverage once measured unchanged sealing at 3.217 microseconds against its
3-microsecond production ceiling; no ceiling or coverage floor was raised.

See `internal/tunnel/l3/bench_gate_test.go`.

---

<div dir="rtl">

## خلاصهٔ فارسی

این صفحه برای کسی است که می‌خواهد کارایی را بهتر کند: اندازه‌گیری‌هایی که یک
سؤال را بسته‌اند، نگه‌داشته شده چون آن سؤال برمی‌گردد.

هر کدام ایده‌ای بوده که درست به‌نظر می‌رسیده. چندتایشان **ساخته نشده‌اند** و
دلیلش یک عدد است نه یک نظر — و نکتهٔ نوشتنشان همین است: بدون این فایل همان ایده
سال بعد دوباره پیشنهاد و دوباره از صفر اندازه‌گیری می‌شود.

اعداد اصلی: تونل زیر بار سنگین **۵۹٪ syscall** است و مسیر دادهٔ لایه‌۳ **۷۸٪
اسمبلیِ ChaCha20-Poly1305** — برای همین PGO چیزی برای راهنمایی‌کردن ندارد، و
اندازه‌گیری هم همین را گفت. بافر رله از ۱۶ به ۶۴ کیلوبایت رفت (یک‌چهارمِ syscall
برای هر گیگابایت) و pool شد. `recvmmsg` و `sendmmsg` ساخته و شیپ شدند. `splice`
اندازه گرفته شد ولی پیش‌فرض خاموش ماند، چون سود روی loopback شاهدِ میدانی نیست.
و دو ایده — MSS خودکار روی تونل معکوس، و batching رلهٔ معکوس — ساخته یا اندازه
گرفته شدند و **بی‌اثر** بودند؛ هر دو با عددشان اینجا ثبت شده‌اند.

بازبینی ۲۰۲۶/۱۱/۰۳: تخصیص موقت مسیر UDP بومی از ۱۰–۱۱ به ۱–۲ مورد برای هر
بسته رسید. بافرهای در اختیار اتصال کم‌ترافیک WS/WSS از ۱۲۸ به ۱۶ کیلوبایت
کاهش یافت و یک goroutine از هر اتصال حذف شد. در WSSMux، خرابی listener باعث
بازسازی همان نسل می‌شود و پایان هر نشست داده، کارهای backend همان نشست را لغو
می‌کند. بهینه‌سازی دریافت مستقیم فریم، پس از مشاهدهٔ افت سرعت و توقف اعلام ظرفیت
آزاد دریافت هنگام انسداد backend، کنار گذاشته شد؛ انتقال به مسیر Read قبلی برگشت.
در نتیجه صرفه‌جویی ۲۸ تا ۳۰ درصدی heap آن آزمایش دیگر بخشی از نسخهٔ نهایی نیست.
افزایش عمومی سرعت یا حذف loss شبکه ادعا نمی‌شود. جزئیات و لینک آزمون‌ها در بالا
آمده‌اند.

</div>

---
[← Back to the docs index](README.md)

---

*Last verified against Backpack v1.26.0 (2026-11-14)*
