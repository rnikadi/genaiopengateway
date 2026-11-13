# Benchmarks

How much latency the gateway adds to a model call, and how many calls one instance serves. The numbers are reproducible with one command, and CI runs a smaller version on every pull request so a regression fails the build.

## Method

`scripts/bench.sh` starts three things on one machine:

1. **A fake upstream** (`test/bench`, `-upstream` mode): an OpenAI-compatible `/v1/chat/completions` that answers a fixed 14-token completion at once, so the upstream adds as little as possible.
2. **The gateway** with [`test/bench/gateway.yaml`](https://github.com/rnikadi/genaiopengateway/blob/main/test/bench/gateway.yaml): one virtual model on that upstream, a static key, prices, and usage metered into SQLite. Every request goes through authentication, policy, the limiter, routing, the upstream call and the usage write, as in production. Body logging, guardrails and the cache are off.
3. **Optional peers**, configured for the same upstream with their defaults.

The load generator (`test/bench`) then sends the same chat completion to each target from `C` concurrent clients with keep-alive connections: a warm-up of 1,000 requests, then `N` measured requests. The first target is the upstream alone, which is the baseline; **added latency** is a target's percentile minus the baseline's at the same percentile.

```
scripts/bench.sh                                    # gateway vs. the upstream alone
PEERS=litellm scripts/bench.sh                      # also LiteLLM (needs `litellm` on PATH)
N=50000 C=64 STREAM=1 FORMAT=markdown scripts/bench.sh
```

## Results

Run on 11 November 2026 on a 4 vCPU Intel Xeon 2.1 GHz cloud VM with Go 1.26, everything on the same host. LiteLLM 1.109.0 ran from `pip install 'litellm[proxy]'` with the config `scripts/bench.sh` writes (a master key, telemetry off, no database), first with its default single worker and then with 4 workers.

**Non-streamed, 20,000 requests, 32 clients:**

| Target | p50 ms | p90 ms | p99 ms | Added p50 ms | Added p99 ms | Requests/s |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| upstream | 0.47 | 1.45 | 3.02 | baseline | baseline | 49,143 |
| GenAIOpenGateway | 2.17 | 4.21 | 7.62 | +1.70 | +4.61 | 12,889 |
| LiteLLM, 1 worker | 326.35 | 370.40 | 414.70 | +325.88 | +411.68 | 97 |

**Non-streamed, 5,000 requests, 32 clients, LiteLLM with 4 workers:**

| Target | p50 ms | p90 ms | p99 ms | Added p50 ms | Added p99 ms | Requests/s |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| upstream | 0.42 | 1.25 | 2.36 | baseline | baseline | 54,752 |
| GenAIOpenGateway | 2.23 | 4.00 | 7.44 | +1.81 | +5.08 | 12,795 |
| LiteLLM, 4 workers | 90.21 | 126.66 | 162.63 | +89.78 | +160.27 | 344 |

**Streamed, 5,000 requests, 32 clients:**

| Target | p50 ms | p90 ms | p99 ms | Added p50 ms | Added p99 ms | Requests/s |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| upstream | 0.44 | 1.41 | 2.99 | baseline | baseline | 51,558 |
| GenAIOpenGateway | 2.89 | 5.63 | 11.02 | +2.45 | +8.03 | 9,718 |
| LiteLLM, 1 worker | 582.48 | 634.03 | 723.78 | +582.04 | +720.78 | 55 |

With the load generator, upstream and gateway sharing four vCPUs, the gateway adds about 2 ms at p50 and 5 ms at p99 while serving about 12,800 requests a second. Against a real provider, whose own latency is hundreds of milliseconds, that is under 1% of a typical call.

## Reading these numbers

- **Same host, shared CPUs.** The load generator and upstream compete with the gateway for the four vCPUs, so absolute throughput on a dedicated host is higher. Compare rows within a table, not across machines.
- **Peers at their defaults.** LiteLLM was not tuned beyond the worker count; more workers, a different event loop or turning off features may improve its numbers. Corrections with a reproducible config are welcome as pull requests to `scripts/bench.sh`.
- **One peer so far.** The roadmap calls for two. Other gateways (Portkey, Envoy AI Gateway, Bifrost, agentgateway) can be added to `scripts/bench.sh` as `PEERS` entries; the load generator's `-target name=url,key=secret,header=Name:Value` covers gateways that route by header.
- **What is not measured.** Guardrails have their own gate (under 10 ms at p50 on a 32 KiB request, [guardrails.md](guardrails.md)); Postgres and Valkey add a network hop each; body logging adds encryption and a write.

## In CI

The `ci` workflow runs `N=2000 C=16 GATE=10ms scripts/bench.sh`, which fails when the gateway adds more than 10 ms at p50 over the upstream. The gate is loose because shared runners are noisy; it catches order-of-magnitude regressions, not small ones.
