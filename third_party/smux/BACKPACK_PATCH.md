# BackPack ready-write experiment

Runtime sources, upstream tests, license and project instructions are copied
from `github.com/xtaci/smux v1.5.57`. The module path and public upstream APIs
are unchanged. `go.mod` pins this local copy to make the small experimental
extension reproducible without editing a shared module cache. The replacement
is version-specific: bumping the upstream requirement cannot silently keep an
older local implementation. Rebase the patch explicitly to retain batching.

Changes: `Session.sendLoop` recognizes an optional connection interface with
`WriteBufferBatchLimits() (frames, bytes int)`. Only a framed WS mux explicitly
configured with `wsmux_write_batch = true` returns nonzero limits. Every other
connection retains the upstream send loop. `shaperQueue.popLimited` preserves
the next round-robin entry when it cannot join a batch; controls leave alone.

`readybatch.go` writes only requests already available, with no timer, worker
queue or asynchronous acknowledgement. At most 16 frames / 64 KiB are joined
by BackPack; an individual maximum upstream frame remains supported unchanged.
Per-request byte/error accounting and borrowed-buffer lifetime are preserved.

Run both upstream and extension tests when updating this copy. Rebase this
small patch explicitly when updating smux, and preserve the upstream license.
