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

Use a test tunnel and an unused port. The echo backend defaults to no listener;
`-serve` binds exactly the supplied address. Keep it on loopback and forward to
it through the tunnel. The benchmark itself generates sustained bidirectional
traffic, so choose a duration that fits your bandwidth budget.

Each target/concurrency runs serially. All connections are established before
timing. Every completed echo must match the random payload byte for byte. The
reported `verified_echo_mbps` counts verified payload bytes **once**, even though
an exchange travels in both directions. It is a round-trip echo workload, not
independent upload/download capacity. Failed or corrupted transfers are counted
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

JSON is a single array, suitable for storing and comparing runs. Connection or
argument failures exit nonzero; per-connection transfer failures remain explicit
in each result's `failures` counter. Ctrl+C closes active sockets and the echo
backend exits cleanly. Limits: 16 targets, 8 concurrency levels, 1..256 connections,
1s..10m per case, and a 1-byte..1-MiB chunk.
