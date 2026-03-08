# Gossip Proxy Optimizations

This document explains the performance optimizations added to `gossip-proxy/main.go` and why they should improve throughput/latency when running two nodes.

## Problem Observed
Your latest two-node result only improved slightly over baseline:

- Baseline RPS: `10.1020`
- Two-node RPS: `9.7504`

That indicates offloading exists but the proxy path still had avoidable overhead and stale balancing signals.

## What Was Changed

## 1) Reused a pooled HTTP client (removed per-request client creation)
Before: each request created `http.Client{Timeout: 30s}`.

After: a single global `upstreamClient` + tuned `http.Transport` is reused.

Why it helps:
- Reuses TCP keep-alive connections.
- Reduces handshake/socket churn under concurrency.
- Lowers proxy-side CPU overhead.

Code location: `gossip-proxy/main.go` (`upstreamTransport`, `upstreamClient`, and `upstreamClient.Do(req)`).

## 2) Replaced lock-based hot path with atomics
Before: each request read CPU load via `RWMutex`.

After: `sync/atomic` for:
- CPU load (`MyCurrentLoadBits`)
- In-flight request count (`LocalInflight`)

Why it helps:
- Less lock contention under high concurrent requests.
- Faster per-request decision logic.

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

## 4) Added tunable balancing flags
New flags:
- `--offload-at` (default `60`)
- `--accept-below` (default `55`)
- `--max-inflight` (default `8`)

Why it helps:
- Lets you tune behavior for your node size and function runtime.
- Avoids hardcoded thresholds that may be wrong for your environment.

## 5) Better peer selection
Before: selected the first peer with load `< 50`.

After: scans peers and chooses the one with the lowest score below `--accept-below`.

Why it helps:
- Reduces random/early suboptimal choices.
- More stable distribution when multiple peers exist.

## 6) Prevented offload ping-pong
Added header `X-Gossip-Offload-Hop`.
- If absent (`0`), request may be offloaded.
- Once offloaded, hop is set to `1`, so receiver does not offload again.

Why it helps:
- Prevents request bouncing between proxies.
- Cuts unnecessary network hops and latency variance.

## 7) Forwarding correctness and protocol cleanup
Improvements:
- Forward URL now uses `RequestURI()` (preserves query string).
- Request uses caller context (`NewRequestWithContext`).
- Drops hop-by-hop headers (`Connection`, `TE`, etc.).

Why it helps:
- Correctness for function endpoints that use query params.
- Fewer proxy-level protocol edge cases under load.

## 8) Faster gossip freshness
Before: monitor and `UpdateNode` every 2s.

After: updates every 500ms.

Why it helps:
- Peer metadata is less stale.
- Routing decisions adapt faster to changing node load.

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

## 6) Tune thresholds if needed
- If Proxy 1 is still overloaded, lower `--offload-at` (e.g., `55 -> 50`).
- If too much offloading happens, raise `--offload-at` or lower `--accept-below`.
- If offload reaction is delayed during bursts, lower `--max-inflight`.

## How To Benchmark the New Version
Use your normal setup, then compare before/after with the same load profile:

e.g. 
Node1 Cluster
go run main.go \
  --target="http://192.168.64.4:8080" \
  --port=8082 \
  --gossip=7947 \
  --join="127.0.0.1" \ 
  --offload-at=70 \
  --accept-below=55 \
  --max-inflight=8

Node2 Cluster
go run main.go \
  --target="http://192.168.64.3:8080" \
  --port=8081 \
  --gossip=7946 \
  --join="127.0.0.1:7947" \
  --offload-at=70 \
  --accept-below=55 \
  --max-inflight=8

```bash
hey -n 100 -c 20 -d "150000" http://localhost:8081/function/stress-test
```

Recommended starting thresholds:
- Busy/entry proxy: `--offload-at=55 --accept-below=50 --max-inflight=6`
- Secondary proxy: `--offload-at=70 --accept-below=55 --max-inflight=8`

Then tune in small steps:
- If local queue still grows: lower `--offload-at` by 3-5.
- If too many remote hops: raise `--offload-at` or lower `--accept-below`.
- If offload happens too late during bursts: lower `--max-inflight`.

## Notes
A binary artifact (`gossip-proxy/gossip-proxy`) may appear after `go build`; this is expected for local validation and not part of logic changes.
