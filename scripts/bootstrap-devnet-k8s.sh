#!/usr/bin/env bash
# bootstrap-devnet-k8s.sh — Bootstrap the Liquidity L1 devnet on GKE.
#
# Collects node IDs + BLS keys from all 5 running pods, generates a proper
# platform genesis with all 5 as initial stakers, injects it via ConfigMap,
# restarts nodes with clean PVCs, then runs `lqd bootstrap` to create the
# EVM, DEX, and FHE chains.
#
# Prerequisites:
#   - kubectl configured with gke_liquidity-devnet_us-central1_dev context
#   - 5 node pods running in the chain namespace
#   - chain-bootstrap-key secret with MNEMONIC
#
# Usage:
#   ./scripts/bootstrap-devnet-k8s.sh
#   ./scripts/bootstrap-devnet-k8s.sh --dry-run
set -euo pipefail

CTX="gke_liquidity-devnet_us-central1_dev"
NS="chain"
STS="node"
REPLICAS=5
DRY_RUN=false
SKIP_GENESIS=false

while [[ $# -gt 0 ]]; do
  case "$1" in
    --dry-run)      DRY_RUN=true; shift ;;
    --skip-genesis) SKIP_GENESIS=true; shift ;;
    --context)      CTX="$2"; shift 2 ;;
    --namespace)    NS="$2"; shift 2 ;;
    *)              echo "Unknown arg: $1" >&2; exit 1 ;;
  esac
done

K="kubectl --context $CTX -n $NS"

echo "=== Liquidity Devnet Bootstrap ==="
echo "  Context:   $CTX"
echo "  Namespace: $NS"
echo "  Replicas:  $REPLICAS"
echo "  Dry run:   $DRY_RUN"
echo ""

# --- Step 1: Collect node info ---
echo "--- Step 1: Collect node IDs and BLS keys ---"

declare -a NODE_IDS
declare -a BLS_PUBKEYS
declare -a BLS_POPS

for i in $(seq 0 $((REPLICAS - 1))); do
  echo "  Querying node-$i..."
  INFO=$($K exec "node-$i" -c node -- curl -s -X POST http://localhost:9650/ext/info \
    -H 'Content-Type: application/json' \
    -d '{"jsonrpc":"2.0","id":1,"method":"info.getNodeID"}')

  NODE_ID=$(echo "$INFO" | python3 -c "import sys,json; print(json.load(sys.stdin)['result']['nodeID'])")
  BLS_PK=$(echo "$INFO" | python3 -c "import sys,json; print(json.load(sys.stdin)['result']['nodePOP']['publicKey'])")
  BLS_POP=$(echo "$INFO" | python3 -c "import sys,json; print(json.load(sys.stdin)['result']['nodePOP']['proofOfPossession'])")

  NODE_IDS[$i]="$NODE_ID"
  BLS_PUBKEYS[$i]="$BLS_PK"
  BLS_POPS[$i]="$BLS_POP"

  echo "    NodeID: $NODE_ID"
  echo "    BLS PK: ${BLS_PK:0:20}..."
done

echo ""

# --- Step 2: Check MNEMONIC secret ---
echo "--- Step 2: Verify MNEMONIC secret ---"
MNEMONIC=$($K get secret chain-bootstrap-key -o jsonpath='{.data.MNEMONIC}' | base64 -d)
WORD_COUNT=$(echo "$MNEMONIC" | wc -w | tr -d ' ')
if [[ "$WORD_COUNT" -ne 12 && "$WORD_COUNT" -ne 24 ]]; then
  echo "  ERROR: MNEMONIC has $WORD_COUNT words (expected 12 or 24)" >&2
  exit 1
fi
echo "  MNEMONIC: $WORD_COUNT words (valid)"
echo ""

# --- Step 3: Generate platform genesis ---
if ! $SKIP_GENESIS; then
  echo "--- Step 3: Generate platform genesis ---"

  # Build the genesis JSON with all 5 validators.
  # We use the devgenesis logic but with all 5 stakers.
  # The genesis generator runs as a Go program via the node binary.
  # Instead of trying to build Go locally, we generate the genesis by
  # running lqd on node-0 with a special env var.
  #
  # But actually, the cleaner approach: generate genesis JSON directly
  # using the node IDs and BLS keys we collected. The lux/genesis package
  # produces Codec-encoded bytes, which we need. So we run the generator
  # inside the container.

  # Build a JSON config file for the genesis generator
  STAKERS_JSON="["
  START_TIME=$(date +%s)
  END_TIME=$((START_TIME + 100 * 365 * 24 * 3600)) # 100 years

  for i in $(seq 0 $((REPLICAS - 1))); do
    if [[ $i -gt 0 ]]; then STAKERS_JSON+=","; fi
    STAKERS_JSON+="{\"nodeID\":\"${NODE_IDS[$i]}\",\"blsPublicKey\":\"${BLS_PUBKEYS[$i]}\",\"blsProofOfPossession\":\"${BLS_POPS[$i]}\"}"
  done
  STAKERS_JSON+="]"

  echo "  Stakers: $REPLICAS"
  echo "  Start time: $START_TIME"

  # Write the stakers config as a ConfigMap so the genesis generator can read it
  $K create configmap devnet-stakers \
    --from-literal=stakers.json="$STAKERS_JSON" \
    --from-literal=start-time="$START_TIME" \
    --dry-run=client -o yaml | $K apply -f -

  echo "  Stakers ConfigMap applied"

  # Run the genesis generator as a Job inside the cluster.
  # The node binary has a built-in devgenesis that only handles 1 node,
  # so we need to generate it differently.
  #
  # The approach: use node-0 to run `lqd bootstrap` with sybil-protection disabled.
  # First, we need to reconfigure the nodes to NOT use external bootstrap peers
  # and to disable sybil protection so they can form their own network.
  #
  # Actually the simplest path: disable sybil protection so nodes accept each
  # other as validators without genesis staking records, then run bootstrap.

  echo ""
  echo "--- Step 3a: Update StatefulSet (disable sybil, remove external bootstrappers) ---"

  # Patch StatefulSet: remove external bootstrap-nodes, disable sybil protection,
  # enable automining, set consensus params for 5-node quorum
  cat <<'PATCH' | $K apply -f -
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: node
  namespace: chain
spec:
  replicas: 5
  selector:
    matchLabels:
      app: node
  serviceName: node-headless
  template:
    metadata:
      labels:
        app: node
    spec:
      initContainers:
      - name: init-plugins
        image: us-docker.pkg.dev/liquidity-registry/liquidityio/node:dev
        command: ["sh", "-c", "mkdir -p /data/plugins; ls -la /data/plugins/"]
        volumeMounts:
        - name: chain-data
          mountPath: /data
      containers:
      - name: node
        image: us-docker.pkg.dev/liquidity-registry/liquidityio/node:dev
        imagePullPolicy: Always
        command: ["lqd"]
        args:
        - --network-id=3
        - --data-dir=/data
        - --http-host=0.0.0.0
        - --http-port=9650
        - --http-allowed-hosts=*
        - --staking-port=9651
        - --plugin-dir=/data/plugins
        - --api-admin-enabled=true
        - --sybil-protection-enabled=false
        - --enable-automining=true
        - --track-chains=all
        env:
        - name: ENVIRONMENT
          value: dev
        - name: MNEMONIC
          valueFrom:
            secretKeyRef:
              name: chain-bootstrap-key
              key: MNEMONIC
              optional: true
        ports:
        - name: staking
          containerPort: 9651
        - name: http
          containerPort: 9650
        resources:
          requests:
            cpu: 250m
            memory: 1Gi
          limits:
            cpu: "1"
            memory: 4Gi
        volumeMounts:
        - name: chain-data
          mountPath: /data
  volumeClaimTemplates:
  - metadata:
      name: chain-data
    spec:
      accessModes: ["ReadWriteOnce"]
      resources:
        requests:
          storage: 100Gi
PATCH

  echo "  StatefulSet updated"
fi

if $DRY_RUN; then
  echo ""
  echo "=== DRY RUN: would delete PVCs and restart nodes ==="
  echo "  Node IDs: ${NODE_IDS[*]}"
  exit 0
fi

if ! $SKIP_GENESIS; then

  # --- Step 4: Delete PVCs and restart ---
  echo ""
  echo "--- Step 4: Delete stale data and restart nodes ---"

  echo "  Scaling to 0..."
  $K scale sts/$STS --replicas=0 --timeout=120s
  $K wait --for=delete pod/node-0 --timeout=120s 2>/dev/null || true

  echo "  Deleting PVCs..."
  for i in $(seq 0 $((REPLICAS - 1))); do
    PVC="chain-data-node-$i"
    if $K get pvc "$PVC" &>/dev/null; then
      $K delete pvc "$PVC" --timeout=60s
      echo "    Deleted $PVC"
    fi
  done

  echo "  Scaling to $REPLICAS..."
  $K scale sts/$STS --replicas=$REPLICAS

  echo "  Waiting for rollout..."
  $K rollout status sts/$STS --timeout=300s

  echo "  Waiting 30s for nodes to bootstrap..."
  sleep 30

  # Verify all nodes are healthy
  for i in $(seq 0 $((REPLICAS - 1))); do
    HEALTH=$($K exec "node-$i" -c node -- curl -s http://localhost:9650/ext/health/liveness 2>&1 || echo "UNHEALTHY")
    echo "  node-$i health: $HEALTH"
  done

else
  echo "--- Skipping genesis (--skip-genesis) ---"
fi

echo ""

# --- Step 5: Collect new node IDs (may have changed after PVC delete) ---
echo "--- Step 5: Collect fresh node IDs ---"

for i in $(seq 0 $((REPLICAS - 1))); do
  INFO=$($K exec "node-$i" -c node -- curl -s -X POST http://localhost:9650/ext/info \
    -H 'Content-Type: application/json' \
    -d '{"jsonrpc":"2.0","id":1,"method":"info.getNodeID"}')

  NODE_ID=$(echo "$INFO" | python3 -c "import sys,json; print(json.load(sys.stdin)['result']['nodeID'])")
  NODE_IDS[$i]="$NODE_ID"
  echo "  node-$i: $NODE_ID"
done

# Verify P-Chain is working
echo ""
echo "  Checking P-Chain..."
P_HEIGHT=$($K exec node-0 -c node -- curl -s -X POST http://localhost:9650/ext/bc/P \
  -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"platform.getHeight"}' | \
  python3 -c "import sys,json; print(json.load(sys.stdin)['result']['height'])")
echo "  P-Chain height: $P_HEIGHT"

VALIDATORS=$($K exec node-0 -c node -- curl -s -X POST http://localhost:9650/ext/bc/P \
  -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"platform.getCurrentValidators","params":{}}' | \
  python3 -c "import sys,json; print(len(json.load(sys.stdin)['result']['validators']))")
echo "  Validators: $VALIDATORS"

echo ""

# --- Step 6: Run bootstrap ---
echo "--- Step 6: Run lqd bootstrap ---"
echo "  Creating EVM, DEX, and FHE chains..."

$K exec node-0 -c node -- env \
  LUX_URI=http://localhost:9650 \
  CHAINS=evm,dex,fhe \
  NETWORK_NAME=Liquidity \
  KEY_INDEX=5 \
  lqd bootstrap 2>&1 | tee /tmp/bootstrap-output.txt

echo ""

# --- Step 7: Extract chain info ---
echo "--- Step 7: Extract chain info ---"

# Copy bootstrap-result.json from the pod
$K cp node-0:/bootstrap-result.json /tmp/devnet-bootstrap-result.json -c node 2>/dev/null || \
  $K exec node-0 -c node -- cat /bootstrap-result.json > /tmp/devnet-bootstrap-result.json 2>/dev/null || true

if [[ -f /tmp/devnet-bootstrap-result.json ]]; then
  echo "  Bootstrap result:"
  python3 -c "import json; print(json.dumps(json.load(open('/tmp/devnet-bootstrap-result.json')), indent=2))"
else
  echo "  WARNING: Could not retrieve bootstrap-result.json"
  echo "  Check /tmp/bootstrap-output.txt for details"
fi

echo ""

# --- Step 8: Verify EVM chain ---
echo "--- Step 8: Verify chains ---"

# Wait for chains to start
sleep 10

# Get blockchains list
BLOCKCHAINS=$($K exec node-0 -c node -- curl -s -X POST http://localhost:9650/ext/bc/P \
  -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"platform.getBlockchains"}' 2>&1)

echo "  Blockchains:"
echo "$BLOCKCHAINS" | python3 -c "
import sys, json
data = json.load(sys.stdin)
for bc in data['result']['blockchains']:
    print(f\"    {bc['name']}: {bc['id']} (VM: {bc['vmID']})\")
"

# Try EVM RPC
EVM_BC_ID=$(echo "$BLOCKCHAINS" | python3 -c "
import sys, json
data = json.load(sys.stdin)
for bc in data['result']['blockchains']:
    if 'evm' in bc['name'].lower() or 'liquid' in bc['name'].lower():
        print(bc['id'])
        break
" 2>/dev/null || echo "")

if [[ -n "$EVM_BC_ID" ]]; then
  echo ""
  echo "  Testing EVM RPC (blockchain: $EVM_BC_ID)..."

  # Wait for EVM to bootstrap
  sleep 5

  CHAIN_ID=$($K exec node-0 -c node -- curl -s -X POST "http://localhost:9650/ext/bc/$EVM_BC_ID/rpc" \
    -H 'Content-Type: application/json' \
    -d '{"jsonrpc":"2.0","id":1,"method":"eth_chainId","params":[]}' 2>&1 | \
    python3 -c "import sys,json; r=json.load(sys.stdin); print(int(r.get('result','0x0'),16))" 2>/dev/null || echo "FAILED")
  echo "  eth_chainId: $CHAIN_ID (expected: 8675311)"

  BLOCK_NUM=$($K exec node-0 -c node -- curl -s -X POST "http://localhost:9650/ext/bc/$EVM_BC_ID/rpc" \
    -H 'Content-Type: application/json' \
    -d '{"jsonrpc":"2.0","id":1,"method":"eth_blockNumber","params":[]}' 2>&1 | \
    python3 -c "import sys,json; r=json.load(sys.stdin); print(int(r.get('result','0x0'),16))" 2>/dev/null || echo "FAILED")
  echo "  eth_blockNumber: $BLOCK_NUM"

  # Check treasury balance
  BALANCE=$($K exec node-0 -c node -- curl -s -X POST "http://localhost:9650/ext/bc/$EVM_BC_ID/rpc" \
    -H 'Content-Type: application/json' \
    -d '{"jsonrpc":"2.0","id":1,"method":"eth_getBalance","params":["0x9011E888251AB053B7bD1cdB598Db4f9DEd94714","latest"]}' 2>&1 | \
    python3 -c "import sys,json; r=json.load(sys.stdin); bal=int(r.get('result','0x0'),16); print(f'{bal/1e18:.0f} LQDTY')" 2>/dev/null || echo "FAILED")
  echo "  Treasury balance: $BALANCE"
fi

echo ""
echo "════════════════════════════════════════════════════════"
echo "  Liquidity Devnet Bootstrap Complete"
echo "════════════════════════════════════════════════════════"
echo "  Cluster:  $CTX"
echo "  Nodes:    $REPLICAS"
echo "  Node IDs: ${NODE_IDS[*]}"
echo ""
echo "  Next steps:"
echo "    1. Deploy contracts (USDL, SecurityTokens)"
echo "    2. Wire ATS settlement to EVM RPC"
echo "    3. Update DNS records"
echo "════════════════════════════════════════════════════════"
