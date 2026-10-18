# Verified concurrent tunnel benchmark

This tool compares already configured **TCP** forwarded ports, using an echo
backend on the remote side. It never creates services, changes routes or tunes
production configurations. Build once on each host:

```sh
go build -o tunnelbench ./tools/tunnelbench
# Kharej: configure each test tunnel to forward to this loopback echo backend.
./tunnelbench -serve 127.0.0.1:9001
# Iran: point to the corresponding forwarded ports, one target per protocol.
./tunnelbench -target tcp=127.0.0.1:18080 -target hy2=127.0.0.1:18081 \
  -parallel 1,8,32 -duration 15s -pid tcp=1234 -pid hy2=5678 -json > results.json
```

For independent continuous upload/download and CPU/RSS during those transfers,
use **bulk mode on both hosts**:

```sh
# Kharej: existing test tunnels forward to this private backend.
./tunnelbench -serve 127.0.0.1:9001 -mode bulk
# Iran: each target runs download, then upload, at each concurrency level.
./tunnelbench -mode bulk -target hy2=127.0.0.1:18081 \
  -parallel 1,8,32 -duration 15s -warmup 2s -io-timeout 5s \
  -pid hy2=5678 -json > bulk-results.json
```

Bulk mode establishes all streams and negotiates fresh random verification material
before starting. It generates a nonrepeating AES-CTR pseudorandom stream from
each session's seed, avoiding repeated blocks that could inflate results on a
compressing carrier. Writers stream continuously, with no per-block echo wait.
Receivers check every byte; upload throughput counts the remote receiver's
cumulative acknowledgements (every 50 ms), not locally queued writes. Download
counts bytes checked locally. Warmup (0..30s, default 2s), connection setup and
handshakes are excluded from throughput and CPU/RSS windows. ACK delivery can lag
receipt by one reporting interval plus path delay. JSON identifies `mode`,
`direction`, `warmup_seconds` and `verified_bulk_mbps`; the echo field stays zero.
Use these bulk fields for capacity comparisons, not `verified_echo_mbps`.
`-chunk` controls echo only; bulk always uses 32 KiB verification patterns.

The I/O timeout bounds a stall in each direction; use the same value on every
comparison. A corrupt payload, invalid ACK, stalled stream or failed handshake
fails the bulk case with a nonzero exit. Each stream must carry verified traffic
in the measured window. Bulk and echo backends are deliberately separate modes:
a successful echo does not impersonate a bulk test. Ctrl+C interrupts handshakes,
warmup and active transfers; the private backend caps a session at 15 minutes.

Use a test tunnel and an unused port. The echo backend defaults to no listener;
`-serve` binds exactly the supplied address. Keep it on loopback and forward to
it through the tunnel. The benchmark itself generates sustained bidirectional
traffic, so choose a duration that fits your bandwidth budget.

Each target/concurrency runs serially. All connections are established before
timing. Every completed echo must match the random payload byte for byte. The
reported `verified_echo_mbps` counts verified payload bytes **once**, even though
an exchange travels in both directions. It is a round-trip echo workload, not
independent upload/download capacity. The per-exchange I/O deadline defaults to 2 seconds; set `-io-timeout 5s`
(or another value in 100ms..1m) when evaluating intentionally slow request
carriers such as Short HTTPS. Use the same deadline for comparisons. The overall
case deadline still bounds the workload. JSON records the chunk size and I/O
budget. Failed or corrupted transfers are counted
and never contribute bytes. Keep payload size, duration, machines, routes and
concurrency identical when comparing protocols. Repeat in reversed target order
and across representative hours to reduce order and path bias.

Repeat `-pid name=PID` for local engine processes to include, grouped by target.
All observed helper descendants are included once. Resource samples are taken
every 250 ms and at the boundaries; CPU is process user+system time, with 100%
meaning one fully used core. Peak memory is the maximum observed aggregate RSS,
which includes shared pages and is not private heap allocation. A missing PID,
permission error or disappearing process increments `sampling_errors`; human
output then marks resource values unmeasured instead of claiming zero usage.
Short-lived processes between samples can be missed. PID creation times separate
reused identities. These are **local** resources; run a corresponding measurement
on the other host to assess its engine/helper costs. Without `-pid`, CPU and RAM
are explicitly unmeasured. This tool does not benchmark native UDP forwarding.

JSON is a single array, suitable for storing and comparing runs. Each case records
its UTC start time so resource samples on both hosts can be correlated. Connection or
argument failures exit nonzero; per-connection transfer failures remain explicit
in each result's `failures` counter. Ctrl+C closes active sockets and the echo
backend exits cleanly. Limits: 16 targets, 8 concurrency levels, 1..256 connections,
1s..10m per case, and a 1-byte..1-MiB chunk.
