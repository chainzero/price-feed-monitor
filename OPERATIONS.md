# Akash BME Price Feed Monitor — Operations

## Table of Contents
1. [Current Deployment](#current-deployment)
2. [Configuration Updates](#configuration-updates)
3. [What Is Monitored](#what-is-monitored)

---

## Current Deployment

The monitor runs as a single-replica Kubernetes Deployment in the `akash-services` namespace.

### Cluster
The service is hosted on the **provider-monitoring** cluster, reachable via Tailscale at `provider-monitoring`. This is the same cluster that hosts the Akash Provider Monitor — the BME monitor shares the infrastructure but runs as an independent workload.

The pod is pinned to the control-plane node (`provider-monitor-k3cluster`), which has the internet egress required to reach the Akash API, Ethereum RPC, Wormholescan, and Slack.

### Manifests
Kubernetes manifests (`deployment.yaml`, `configmap.yaml`) are maintained in the [`akash.bme.monitor`](https://github.com/ovrclk/server-mgmt/tree/main/mainnet/deployments/akash.bme.monitor) directory of the server-mgmt repo.

### Key design notes
- **Single replica by design.** Alert cooldown state is held in memory. Running multiple instances would produce duplicate Slack alerts.
- **Stateless.** No persistent volumes. The pod can be safely force-deleted and rescheduled without data loss.
- **Config via ConfigMap.** All tunable parameters (poll intervals, thresholds, networks, relayers) live in the ConfigMap — no code change or image rebuild required for config updates.
- **Secret injection.** The Slack webhook URL is injected at runtime from a K8s Secret (`price-feed-monitor-secrets`) and never stored in the ConfigMap or image.

### Current image
```
scarruthers/price-feed-monitor:v8
```

---

## Configuration Updates

### Change config only (no new image needed)
Applies to: adding/removing networks or relayers, adjusting poll intervals or alert thresholds, updating wallet addresses or health endpoints, changing report schedule.

```bash
kubectl apply -f deploy/configmap.yaml
kubectl rollout restart deployment/price-feed-monitor -n akash-services
```

### Deploy a new image version
Applies to: code changes to alert logic, new monitoring components, bug fixes.

```bash
# Build and push
docker buildx build --platform linux/amd64,linux/arm64 \
  --tag scarruthers/price-feed-monitor:vN --push .

# Update image tag in deploy/deployment.yaml, then:
kubectl apply -f deploy/configmap.yaml
kubectl apply -f deploy/deployment.yaml
kubectl rollout status deployment/price-feed-monitor -n akash-services
```

---

## What Is Monitored

### Scheduled Health Reports
Full status summary posted to `#akash-bme-monitor` at **08:00 and 20:00 CST** daily, and on every pod restart.

---

### 1. Oracle Price Health
**Poll:** every 60s

Fetches the latest AKT/USD price from the Akash oracle module and checks how old it is.

| Age | Severity |
|-----|----------|
| > 5 min | Warning |
| > 15 min | Critical |
| > 30 min | Emergency |

---

### 2. Hermes Relayer Health
**Poll:** every 30s — alerts after **3 consecutive failures**

For each relayer (`hermes-relayer-01/02/03`):
- `/health` endpoint reachable and `isRunning = true`
- Wallet balance vs. thresholds (Info: < 1000 AKT, Warning: < 500 AKT, Critical: < 100 AKT)
- `contractAddress` and `priceFeedId` match expected values (misconfiguration detection)

---

### 3. Guardian Set — Ethereum RPC
**Poll:** every 10 min

Reads the current Wormhole guardian set directly from the Ethereum contract (`0x98f3c9e6...`) and compares the guardian addresses against what the Akash Wormhole CosmWasm contract holds on-chain. Alerts if the sets are out of sync.

---

### 4. Guardian Set — Wormholescan
**Poll:** every 30 min

Secondary detection path via the Wormholescan REST API. Monitors for guardian set index changes and retrieves the governance VAA needed for on-chain submission if an upgrade occurs. Complements Component 3 — does not require an Ethereum RPC.

---

### 5. BME (Burn Mint Equilibrium) Status
**Poll:** every 60s

Monitors the Akash token burn/mint mechanism. Three independent checks:

**Collateral ratio vs. chain-defined thresholds** (thresholds read dynamically from chain):

| Consecutive polls breaching | Severity |
|-----------------------------|----------|
| 1 | Warning |
| 2 | Critical |
| 3 | Emergency (final) |

**Minting or refunds halted** (`mints_allowed` / `refunds_allowed`):

| Consecutive polls halted | Severity |
|--------------------------|----------|
| 1 | Warning |
| 2 | Critical |
| 3 | Emergency (final) |

Alert includes halt reason decoded from the chain status field (e.g. oracle staleness vs. collateral breach).

**Inconsistent API response** (ratio = 0 with healthy status): Warning with explanation — threshold alerts suppressed for that poll cycle.

---

### 6. Guardian Update Announcements
**Poll:** every 30 min

Monitors the [Pyth Network governance forum](https://forum.pyth.network/c/proposals/7.rss) RSS feed for posts matching keywords: `guardian`, `guardian set`, `wormhole core`, `guardian rotation`, `guardian key`. Alerts on first match per post.
