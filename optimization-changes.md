# Gossip Proxy Optimizations (Detailed)

This document explains the performance optimizations added to `gossip-proxy/main.go` and why they should improve throughput/latency when running two nodes.

## Problem Observed
Your latest two-node result only improved slightly over baseline:

- Baseline RPS: `10.1020`
- Two-node RPS: `9.7504`

That indicates offloading exists but the proxy path still had avoidable overhead and stale balancing signals.

## What Was Changed (Detailed)

## 1) Reused a pooled HTTP client (removed per-request client creation)
Before: each request created `http.Client{Timeout: 30s}`.

After: a single global `upstreamClient` + tuned `http.Transport` is reused.

Why it helps:
- Reuses TCP keep-alive connections.
- Reduces handshake/socket churn under concurrency.
- Lowers proxy-side CPU overhead.
- Avoids repeated allocation/GC pressure from short-lived clients/transports.

Technical detail:
- In Go, the `Transport` owns connection pooling behavior. Creating a new client (and implicitly a new transport policy) in the hot path prevents effective socket reuse.
- The new transport (`MaxIdleConns`, `MaxIdleConnsPerHost`, `IdleConnTimeout`) keeps warm connections to OpenFaaS gateways, which is important when `hey -c 20` sends bursty concurrent traffic.
- This primarily improves p50/p95 latency and can improve RPS when proxy overhead was non-trivial.

Tradeoff:
- Higher idle-connection limits consume a bit more memory/file descriptors. For a 2-node lab environment this is usually acceptable.

Code location: `gossip-proxy/main.go` (`upstreamTransport`, `upstreamClient`, and `upstreamClient.Do(req)`).

## 2) Replaced lock-based hot path with atomics
Before: each request read CPU load via `RWMutex`.

After: `sync/atomic` for:
- CPU load (`MyCurrentLoadBits`)
- In-flight request count (`LocalInflight`)

Why it helps:
- Less lock contention under high concurrent requests.
- Faster per-request decision logic.

Technical detail:
- Request handling is a hot path. Even read locks (`RLock`) can become expensive when many goroutines frequently contend.
- CPU load and inflight count are simple scalar state, which is a good fit for lock-free atomic load/store operations.
- The code now tracks CPU as `float64` encoded in `uint64` bits (`math.Float64bits` + atomic ops), and inflight as an atomic `int64`.

Tradeoff:
- Atomic state is less expressive than richer lock-protected structs; keep this pattern only for simple counters/metrics.

## 3) Added in-flight aware load score
Before: offload decision used only CPU threshold (`> 50%`).

After: local score combines CPU + queue pressure:

```text
score = 0.7 * cpu + 0.3 * queuePressure
queuePressure = min(100, inflight * 100 / maxInflight)
```

Why it helps:
- Reacts sooner to bursts when requests queue up before CPU metric catches up.
- Improves routing decisions for short traffic spikes.

Technical detail:
- CPU metrics are sampled and can lag behind sudden traffic bursts.
- Inflight count is immediate: if inflight grows quickly, queue pressure rises even before CPU average reflects it.
- Weighted score design:
  - CPU (`70%`) keeps decision aligned with real compute saturation.
  - Queue pressure (`30%`) gives early warning for congestion.
- This improves stability versus a binary CPU-only threshold that can under-react, then over-react.

Tuning notes:
- Increase CPU weight if requests are long-running and CPU-bound.
- Increase queue weight if traffic is very bursty and you see periodic queue buildup.

## 4) Added tunable balancing flags
New flags:
- `--offload-at` (default `60`)
- `--accept-below` (default `55`)
- `--max-inflight` (default `8`)

Why it helps:
- Lets you tune behavior for your node size and function runtime.
- Avoids hardcoded thresholds that may be wrong for your environment.

Technical detail:
- `--offload-at`: sender-side trigger. Lower value means earlier offload.
- `--accept-below`: receiver-side quality gate. Lower value means “only offload to clearly idle peers.”
- `--max-inflight`: queue-pressure normalization. Smaller value makes queue pressure rise faster.

Parameter interaction:
- If `accept-below` is too close to `offload-at`, offload can become noisy.
- Good starting rule: keep `accept-below` about `5` lower than `offload-at`.

## 5) Better peer selection
Before: selected the first peer with load `< 50`.

After: scans peers and chooses the one with the lowest score below `--accept-below`.

Why it helps:
- Reduces random/early suboptimal choices.
- More stable distribution when multiple peers exist.

Technical detail:
- “First match” depends on memberlist ordering and can route repeatedly to a mediocre peer.
- “Best score under threshold” is greedy but effective for small clusters, and avoids centralized coordination.
- In 2-node setup, this still helps because it enforces a stricter “only if peer is truly better” policy.

## 6) Prevented offload ping-pong
Added header `X-Gossip-Offload-Hop`.
- If absent (`0`), request may be offloaded.
- Once offloaded, hop is set to `1`, so receiver does not offload again.

Why it helps:
- Prevents request bouncing between proxies.
- Cuts unnecessary network hops and latency variance.

Technical detail:
- Without a hop guard, Node A may offload to Node B, and Node B could immediately offload back if its local threshold is crossed.
- One extra hop can multiply latency and increase request amplification in stress scenarios.
- The one-hop policy guarantees bounded forwarding depth (`<= 1`), which is important for predictable tail latency.

## 7) Forwarding correctness and protocol cleanup
Improvements:
- Forward URL now uses `RequestURI()` (preserves query string).
- Request uses caller context (`NewRequestWithContext`).
- Drops hop-by-hop headers (`Connection`, `TE`, etc.).

Why it helps:
- Correctness for function endpoints that use query params.
- Fewer proxy-level protocol edge cases under load.

Technical detail:
- `RequestURI()` keeps query strings intact (e.g., `/function/x?a=1&b=2`), avoiding accidental behavior changes.
- `NewRequestWithContext` lets upstream calls cancel quickly if the client disconnects or times out.
- Hop-by-hop headers are connection-local by HTTP spec and should not be forwarded by proxies; removing them avoids subtle bugs.

## 8) Faster gossip freshness
Before: monitor and `UpdateNode` every 2s.

After: updates every 500ms.

Why it helps:
- Peer metadata is less stale.
- Routing decisions adapt faster to changing node load.

Technical detail:
- At 2s intervals, a short benchmark window can spend a large fraction of time making decisions on old load values.
- Reducing to 500ms improves convergence speed in a small cluster.
- This is a tradeoff between control-plane chatter and routing freshness; for 2 nodes, 500ms is usually a good balance.

## Build Validation
The updated proxy compiles successfully:

```bash
cd gossip-proxy
go build ./...
```

## Steps To Run The Optimized Implementation

## 1) Build the proxy
From project root:

```bash
cd gossip-proxy
go build ./...
```

## 2) Start Proxy 1 (Terminal A)
Use the proxy in front of node `192.168.64.3:8080`:

```bash
cd gossip-proxy
go run main.go \
  --target="http://192.168.64.3:8080" \
  --port=8081 \
  --gossip=7946 \
  --join="127.0.0.1:7947" \
  --offload-at=55 \
  --accept-below=50 \
  --max-inflight=6
```

## 3) Start Proxy 2 (Terminal B)
Use the proxy in front of node `192.168.64.4:8080`:

```bash
cd gossip-proxy
go run main.go \
  --target="http://192.168.64.4:8080" \
  --port=8082 \
  --gossip=7947 \
  --join="127.0.0.1:7946" \
  --offload-at=70 \
  --accept-below=55 \
  --max-inflight=8
```

## 4) Send load to Proxy 1 (Terminal C)

```bash
hey -n 100 -c 20 -d "150000" http://localhost:8081/function/stress-test
```

## 5) Compare results
Capture output in `result.md` and compare:
- `Requests/sec`
- `Average`
- `Slowest`
- Error rate (`Non-2xx` / timeouts)

## 6) Tune thresholds if needed
- If Proxy 1 is still overloaded, lower `--offload-at` (e.g., `55 -> 50`).
- If too much offloading happens, raise `--offload-at` or lower `--accept-below`.
- If offload reaction is delayed during bursts, lower `--max-inflight`.

Recommended tuning loop (practical):
1. Keep `-n`, `-c`, payload, and function image fixed.
2. Run each config 3 times; record median `Requests/sec` and `Slowest`.
3. Change only one flag at a time.
4. Stop when RPS gains are small (<3%) or tail latency worsens.

## How To Benchmark the New Version
Use the exact commands in the `Steps To Run The Optimized Implementation` section, then run:

```bash
hey -n 100 -c 20 -d "150000" http://localhost:8081/function/stress-test
```

For fair comparison:
1. Keep payload, concurrency, and total requests identical across runs.
2. Compare median of 3 runs, not a single run.
3. Track `Requests/sec`, `Average`, `Slowest`, and error rate.
4. Change one tuning flag at a time.

## Notes
A binary artifact (`gossip-proxy/gossip-proxy`) may appear after `go build`; this is expected for local validation and not part of logic changes.

## Code Map
For quick reference, optimized logic is in:
- `gossip-proxy/main.go` transport/client reuse
- `gossip-proxy/main.go` atomic load/inflight tracking
- `gossip-proxy/main.go` score-based offload decision
- `gossip-proxy/main.go` best-peer selection
- `gossip-proxy/main.go` one-hop anti-ping-pong header
- `gossip-proxy/main.go` faster monitor/update cadence
