# price-feed-monitor

A Go service that monitors the health of the Akash Network BME (Burn Mint Equilibrium) price feed pipeline and sends alerts to Slack.

## What it monitors

| Component | Description |
|---|---|
| **Oracle Price Health** | Polls the Akash oracle REST API for AKT/USD price freshness. Alerts at Warning (5m), Critical (15m), and Emergency (30m) staleness thresholds. |
| **Hermes Relayer Health** | Polls each relayer's `/health` endpoint. Checks `isRunning`, validates `priceFeedId` and `contractAddress` against expected values. |
| **Wallet Balance** | Checks the relayer wallet balance on every oracle poll cycle. Three alert tiers: Info (<1000 AKT), Warning (<500 AKT), Critical (<100 AKT). |
| **Guardian Set — Ethereum RPC** | Queries the Wormhole contract on Ethereum mainnet for the authoritative guardian set index and addresses. Compares against Akash oracle params. |
| **Guardian Set — Wormholescan** | Polls Wormholescan REST API for the global guardian set index. Detects rotations and retrieves the governance VAA needed for on-chain submission. |
| **BME Status** | Polls `/akash/bme/v1/status` for collateral ratio, mints_allowed, and refunds_allowed. Thresholds are read dynamically from the chain. |
| **Pyth Forum** | Monitors the Pyth Network governance RSS feed for guardian-related proposals. |

All monitors use escalating severity (Info → Warning → Critical → Emergency). Unreachable endpoints alert on the first 3 consecutive failures then go silent until recovery.

## Alert behaviour

- **Startup**: Posts a full status summary to Slack when the service starts.
- **Daily**: Posts a full status summary at 08:00 America/Chicago each day.
- **On-event**: Each monitor sends alerts independently as conditions are detected, and sends a resolved message when they clear.
- **Flood protection**: Unreachable-endpoint alerts fire on failures 1, 2, and 3 (escalating severity), then stop until the endpoint recovers.

## Repository layout

```
.
├── main.go                          # Wires and starts all monitors
├── config.yaml                      # Local development config (see Configuration)
├── Dockerfile                       # Multi-stage build → distroless runtime (~8MB)
├── deploy/
│   ├── namespace.yaml               # akash-services namespace
│   ├── configmap.yaml               # config.yaml mounted into the pod
│   ├── secret.yaml                  # Template — contains placeholder, NOT real credentials
│   └── deployment.yaml              # K8s Deployment manifest
└── internal/
    ├── alerting/                    # Slack Incoming Webhook client with cooldown/dedup
    ├── announcements/               # Pyth forum RSS monitor
    ├── bme/                         # Component 6: BME status monitor
    ├── config/                      # Config loading and validation
    ├── guardian/                    # Components 3 & 5: Ethereum RPC + Wormholescan monitors
    ├── hermes/                      # Component 2: Hermes relayer health monitor
    ├── oracle/                      # Component 1: Oracle price + wallet balance monitor
    ├── report/                      # Startup and daily health check messages
    └── types/                       # Shared types (Alert, Severity, API response structs)
```

## Prerequisites

- Go 1.25+
- Docker with `buildx` (for multi-platform builds)
- A Slack Incoming Webhook URL
- `kubectl` access to the target cluster

## Configuration

Configuration is driven by `config.yaml`. Copy the repo's `config.yaml` and edit for your environment. The only required secret is the Slack webhook URL — all other values are set in the config file.

### Environment variable overrides

| Variable | Description |
|---|---|
| `SLACK_WEBHOOK_URL` | Overrides `slack.webhook_url` in config. Always set this via env/secret rather than committing the real URL. |
| `PRICE_FEED_MONITOR_CONFIG` | Path to the config file. Defaults to `/etc/price-feed-monitor/config.yaml`. |

### Adding a relayer

Add entries under `networks[].hermes_relayers` in `config.yaml` / `deploy/configmap.yaml`. No code changes or image rebuild required — the monitors loop over however many relayers are configured.

```yaml
networks:
  - name: mainnet
    akash_api: "https://api.akash.pub"
    hermes_relayers:
      - name: hermes-relayer-01
        health_endpoint: "https://hermesrelayerprod1.akash.pub/health"
        wallet: "akash1..."
        info_wallet_balance: 1000000000   # 1000 AKT — Info alert
        warn_wallet_balance: 500000000    # 500 AKT  — Warning alert
        min_wallet_balance: 100000000     # 100 AKT  — Critical alert
        expected_price_feed_id: "0x4ea5..."
        expected_contract_address: "akash1..."
      - name: hermes-relayer-02
        health_endpoint: "https://hermesrelayerprod2.akash.pub/health"
        # ...
```

## Running locally

```bash
# Set the webhook URL
export SLACK_WEBHOOK_URL="https://hooks.slack.com/services/YOUR/REAL/WEBHOOK"

# Point at the local config
export PRICE_FEED_MONITOR_CONFIG=./config.yaml

go run .
```

## Building the Docker image

Multi-platform build (required if your dev machine is Apple Silicon and the cluster is amd64):

```bash
docker buildx build \
  --platform linux/amd64,linux/arm64 \
  -t scarruthers/price-feed-monitor:v4 \
  -t scarruthers/price-feed-monitor:latest \
  --push .
```

Single-platform local build (faster, for testing):

```bash
docker build -t price-feed-monitor:local .
docker run --rm \
  -e SLACK_WEBHOOK_URL="https://hooks.slack.com/services/YOUR/REAL/WEBHOOK" \
  -v $(pwd)/config.yaml:/etc/price-feed-monitor/config.yaml:ro \
  price-feed-monitor:local
```

## Deploying to Kubernetes

### First-time setup

```bash
# 1. Create the namespace
kubectl apply -f deploy/namespace.yaml

# 2. Create the Slack webhook secret
#    Option A: from the command line (preferred — avoids writing the URL to disk)
kubectl create secret generic price-feed-monitor-secrets \
  --namespace akash-services \
  --from-literal=slack-webhook-url="https://hooks.slack.com/services/YOUR/REAL/WEBHOOK"

#    Option B: edit deploy/secret.yaml with the real URL, then apply
kubectl apply -f deploy/secret.yaml

# 3. Apply the ConfigMap (contains config.yaml)
kubectl apply -f deploy/configmap.yaml

# 4. Apply the Deployment
kubectl apply -f deploy/deployment.yaml
```

### Updating config (no image rebuild needed)

```bash
# Edit deploy/configmap.yaml, then:
kubectl apply -f deploy/configmap.yaml
kubectl rollout restart deployment/price-feed-monitor -n akash-services
```

### Updating the image

```bash
# Build and push new image (see Building section above), then update the tag in deployment.yaml:
kubectl apply -f deploy/deployment.yaml
kubectl rollout status deployment/price-feed-monitor -n akash-services
```

### Checking status

```bash
kubectl get pods -n akash-services -l app=price-feed-monitor
kubectl logs -n akash-services -l app=price-feed-monitor --tail=50 -f
```

## Security notes

- The `deploy/secret.yaml` in this repo contains a **placeholder value only**. Never commit the real webhook URL.
- The pod runs as non-root (uid 65532, distroless image) with a read-only root filesystem.
- The NGINX reverse proxy on each relayer host restricts `/health` access to a whitelist of monitoring server IPs. All other paths return 404.
- `capabilities: drop ALL` is intentionally omitted in the K8s securityContext — it breaks UDP return traffic on this k3s/Flannel cluster, preventing DNS resolution.

## Wormhole guardian set rotation response

If a guardian set rotation is detected, the monitor will post a Critical Slack alert with:
- The new global guardian set index
- A truncated governance VAA
- The full VAA retrieval command

To apply the update:

```bash
# 1. Retrieve the full VAA
curl -s "https://api.wormholescan.io/api/v1/vaas/1/0000000000000000000000000000000000000000000000000000000000000004?pageSize=5" \
  | jq -r '.data[0].vaa'

# 2. Submit to the Akash Wormhole contract
akash tx wasm execute <contract_address> \
  '{"submit_v_a_a":{"vaa":"<base64_vaa>"}}' \
  --from <key> --chain-id akashnet-2 --gas auto --gas-adjustment 1.5

# 3. Confirm oracle params updated
curl -s https://testnetoracleapi.akashnet.net/akash/oracle/v1/params | jq .
```

Once the update is applied, the monitor will automatically send a resolved message on the next poll cycle.
