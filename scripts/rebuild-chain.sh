#!/usr/bin/env bash
# rebuild-chain.sh — Rebuild a Liquidity EVM chain from Avalanche snapshot.
#
# Generates genesis alloc from the latest snapshot, stops chain nodes,
# injects alloc into genesis, restarts nodes, and verifies balances.
#
# Usage:
#   ./scripts/rebuild-chain.sh --env mainnet --snapshot /path/to/SNAPSHOT.json
#   ./scripts/rebuild-chain.sh --env devnet  --snapshot /path/to/SNAPSHOT.json --dry-run
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
GENESIS_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"

# --- Defaults ---
ENV=""
SNAPSHOT=""
DRY_RUN=false
CHAIN_CONTEXT="gke_liquidity-chains_us-central1_chain"
DEPLOYER="0x9011E888251AB053B7bD1cdB598Db4f9DEd94714"
CONTRACTS_DIR="${CONTRACTS_DIR:-$HOME/work/liquidity/contracts}"
STATE_DIR="${STATE_DIR:-$HOME/work/liquidity/state}"

# --- Chain config per environment ---
declare -A CHAIN_IDS
CHAIN_IDS[devnet]=8675311
CHAIN_IDS[testnet]=8675310
CHAIN_IDS[mainnet]=8675309

declare -A NAMESPACES
NAMESPACES[devnet]=devnet
NAMESPACES[testnet]=testnet
NAMESPACES[mainnet]=mainnet

declare -A NODE_PORTS
NODE_PORTS[devnet]=19631
NODE_PORTS[testnet]=19631
NODE_PORTS[mainnet]=29631

# --- Parse args ---
while [[ $# -gt 0 ]]; do
  case "$1" in
    --env)       ENV="$2"; shift 2 ;;
    --snapshot)  SNAPSHOT="$2"; shift 2 ;;
    --dry-run)   DRY_RUN=true; shift ;;
    --context)   CHAIN_CONTEXT="$2"; shift 2 ;;
    *)           echo "Unknown arg: $1" >&2; exit 1 ;;
  esac
done

if [[ -z "$ENV" ]]; then
  echo "error: --env required (devnet|testnet|mainnet)" >&2
  exit 1
fi

if [[ -z "$SNAPSHOT" ]]; then
  echo "error: --snapshot required" >&2
  exit 1
fi

if [[ ! -f "$SNAPSHOT" ]]; then
  echo "error: snapshot not found: $SNAPSHOT" >&2
  exit 1
fi

CHAIN_ID="${CHAIN_IDS[$ENV]}"
NAMESPACE="${NAMESPACES[$ENV]}"
NODE_PORT="${NODE_PORTS[$ENV]}"

echo "=== Rebuild Liquidity EVM ==="
echo "  Environment: $ENV"
echo "  Chain ID:    $CHAIN_ID"
echo "  Namespace:   $NAMESPACE"
echo "  Snapshot:    $SNAPSHOT"
echo "  Dry run:     $DRY_RUN"
echo ""

# --- Step 1: Build genesis-alloc tool ---
echo "--- Step 1: Build genesis-alloc ---"
cd "$GENESIS_DIR"
go build -o /tmp/genesis-alloc ./cmd/genesis-alloc/
echo "  Built /tmp/genesis-alloc"

# --- Step 2: Collect contract bytecodes from existing chain ---
# If we can reach the chain, extract bytecodes for accurate genesis.
# Otherwise, genesis-alloc will output entries without code (storage-only).
SECURITY_BYTECODE=""
USDL_BYTECODE=""
REGISTRY_BYTECODE=""

MANIFEST="$CONTRACTS_DIR/deployments/${ENV}-20260327.json"
HORSE_MANIFEST="$CONTRACTS_DIR/deployments/horse-deploy-${ENV}-1775784137.json"

MANIFEST_FLAGS=""
if [[ -f "$MANIFEST" ]]; then
  MANIFEST_FLAGS="--manifest $MANIFEST"
  echo "  Using manifest: $MANIFEST"

  # Try to extract bytecodes from chain
  if command -v cast &>/dev/null && ! $DRY_RUN; then
    echo "  Extracting bytecodes via port-forward..."
    # Start port-forward in background
    kubectl --context "$CHAIN_CONTEXT" -n "$NAMESPACE" \
      port-forward pod/node-0 "${NODE_PORT}:9631" &>/dev/null &
    PF_PID=$!
    sleep 2

    # Get blockchain ID from node
    BLOCKCHAIN_ID=$(curl -s "http://localhost:${NODE_PORT}/ext/info" \
      -X POST -H 'Content-Type: application/json' \
      -d '{"jsonrpc":"2.0","method":"info.getBlockchains","params":{},"id":1}' \
      2>/dev/null | python3 -c "
import sys, json
data = json.load(sys.stdin)
for bc in data.get('result',{}).get('blockchains',[]):
    if 'evm' in bc.get('name','').lower() or bc.get('vmID','') == 'srEXiWaHuhNyGwPUi444Tu47ZEDwxTWrbQiuD7FmgSAQ6X7Dy':
        print(bc['id'])
        break
" 2>/dev/null || echo "")

    if [[ -n "$BLOCKCHAIN_ID" ]]; then
      RPC="http://localhost:${NODE_PORT}/ext/bc/${BLOCKCHAIN_ID}/rpc"

      # Extract a sample SecurityToken bytecode
      SAMPLE_ADDR=$(python3 -c "
import json
with open('$HORSE_MANIFEST') as f:
    m = json.load(f)
addrs = list(m.get('deployed',{}).values())
print(addrs[0] if addrs else '')
" 2>/dev/null || echo "")

      if [[ -n "$SAMPLE_ADDR" ]]; then
        SECURITY_BYTECODE=$(cast code --rpc-url "$RPC" "$SAMPLE_ADDR" 2>/dev/null || echo "")
      fi

      # Extract USDL bytecode
      USDL_ADDR=$(python3 -c "
import json
with open('$MANIFEST') as f:
    m = json.load(f)
print(m.get('core',{}).get('USDLToken',''))
" 2>/dev/null || echo "")

      if [[ -n "$USDL_ADDR" ]]; then
        USDL_BYTECODE=$(cast code --rpc-url "$RPC" "$USDL_ADDR" 2>/dev/null || echo "")
      fi

      # Extract ComplianceRegistry bytecode
      REG_ADDR=$(python3 -c "
import json
with open('$MANIFEST') as f:
    m = json.load(f)
print(m.get('core',{}).get('ComplianceRegistry',''))
" 2>/dev/null || echo "")

      if [[ -n "$REG_ADDR" ]]; then
        REGISTRY_BYTECODE=$(cast code --rpc-url "$RPC" "$REG_ADDR" 2>/dev/null || echo "")
      fi
    fi

    kill "$PF_PID" 2>/dev/null || true
  fi
fi

HORSE_FLAGS=""
if [[ -f "$HORSE_MANIFEST" ]]; then
  HORSE_FLAGS="--horse-manifest $HORSE_MANIFEST"
  echo "  Using horse manifest: $HORSE_MANIFEST"
fi

# --- Step 3: Generate genesis alloc ---
echo ""
echo "--- Step 3: Generate genesis alloc ---"
ALLOC_OUTPUT="/tmp/genesis-alloc-${ENV}.json"

BYTECODE_FLAGS=""
[[ -n "$SECURITY_BYTECODE" ]] && BYTECODE_FLAGS="$BYTECODE_FLAGS --security-bytecode $SECURITY_BYTECODE"
[[ -n "$USDL_BYTECODE" ]]     && BYTECODE_FLAGS="$BYTECODE_FLAGS --usdl-bytecode $USDL_BYTECODE"
[[ -n "$REGISTRY_BYTECODE" ]] && BYTECODE_FLAGS="$BYTECODE_FLAGS --registry-bytecode $REGISTRY_BYTECODE"

/tmp/genesis-alloc \
  --snapshot "$SNAPSHOT" \
  --output "$ALLOC_OUTPUT" \
  --chain-id "$CHAIN_ID" \
  $MANIFEST_FLAGS \
  $HORSE_FLAGS \
  $BYTECODE_FLAGS

echo "  Alloc written to: $ALLOC_OUTPUT"

if $DRY_RUN; then
  echo ""
  echo "=== DRY RUN: stopping here ==="
  echo "  Review alloc at: $ALLOC_OUTPUT"
  echo "  Entry count: $(python3 -c "import json; print(len(json.load(open('$ALLOC_OUTPUT'))['alloc']))")"
  exit 0
fi

# --- Step 4: Merge alloc into genesis ---
echo ""
echo "--- Step 4: Merge alloc into chain genesis ---"
GENESIS_FILE="$GENESIS_DIR/chains/evm.json"

# Update chain ID in genesis
python3 -c "
import json, sys

with open('$GENESIS_FILE') as f:
    genesis = json.load(f)

with open('$ALLOC_OUTPUT') as f:
    alloc_data = json.load(f)

# Set chain ID
genesis['config']['chainId'] = $CHAIN_ID

# Merge alloc (new alloc replaces old)
genesis['alloc'] = {}
for addr, entry in alloc_data['alloc'].items():
    # Genesis format uses addresses without 0x prefix
    clean_addr = addr.lower().replace('0x', '')
    genesis['alloc'][clean_addr] = entry

with open('$GENESIS_FILE', 'w') as f:
    json.dump(genesis, f, indent=2)

print(f'  Merged {len(alloc_data[\"alloc\"])} entries into genesis')
"

# --- Step 5: Stop chain nodes ---
echo ""
echo "--- Step 5: Stop chain nodes ---"
echo "  Scaling node StatefulSet to 0..."
kubectl --context "$CHAIN_CONTEXT" -n "$NAMESPACE" \
  scale statefulset node --replicas=0 --timeout=120s

echo "  Waiting for pods to terminate..."
kubectl --context "$CHAIN_CONTEXT" -n "$NAMESPACE" \
  wait --for=delete pod/node-0 --timeout=120s 2>/dev/null || true

# --- Step 6: Inject genesis into ConfigMap ---
echo ""
echo "--- Step 6: Update genesis ConfigMap ---"
kubectl --context "$CHAIN_CONTEXT" -n "$NAMESPACE" \
  create configmap evm-genesis \
  --from-file=evm.json="$GENESIS_FILE" \
  --dry-run=client -o yaml | \
kubectl --context "$CHAIN_CONTEXT" -n "$NAMESPACE" apply -f -

echo "  ConfigMap updated"

# --- Step 7: Clear chain data and restart ---
echo ""
echo "--- Step 7: Clear chain data and restart nodes ---"

# Delete PVCs to force fresh chain from new genesis
for i in 0 1 2 3 4; do
  PVC="data-node-$i"
  if kubectl --context "$CHAIN_CONTEXT" -n "$NAMESPACE" get pvc "$PVC" &>/dev/null; then
    echo "  Deleting PVC: $PVC"
    kubectl --context "$CHAIN_CONTEXT" -n "$NAMESPACE" delete pvc "$PVC" --timeout=60s
  fi
done

echo "  Scaling node StatefulSet to 5..."
kubectl --context "$CHAIN_CONTEXT" -n "$NAMESPACE" \
  scale statefulset node --replicas=5

echo "  Waiting for nodes to become ready..."
kubectl --context "$CHAIN_CONTEXT" -n "$NAMESPACE" \
  rollout status statefulset/node --timeout=300s

# --- Step 8: Verify balances ---
echo ""
echo "--- Step 8: Verify balances ---"

# Port-forward to new chain
kubectl --context "$CHAIN_CONTEXT" -n "$NAMESPACE" \
  port-forward pod/node-0 "${NODE_PORT}:9631" &>/dev/null &
PF_PID=$!
sleep 5

# Get new blockchain ID
BLOCKCHAIN_ID=$(curl -s "http://localhost:${NODE_PORT}/ext/info" \
  -X POST -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","method":"info.getBlockchains","params":{},"id":1}' \
  | python3 -c "
import sys, json
data = json.load(sys.stdin)
for bc in data.get('result',{}).get('blockchains',[]):
    if 'evm' in bc.get('name','').lower():
        print(bc['id'])
        break
" 2>/dev/null || echo "")

if [[ -z "$BLOCKCHAIN_ID" ]]; then
  echo "  WARNING: Could not determine blockchain ID. Manual verification needed."
  kill "$PF_PID" 2>/dev/null || true
  exit 0
fi

RPC="http://localhost:${NODE_PORT}/ext/bc/${BLOCKCHAIN_ID}/rpc"
echo "  RPC: $RPC"

# Verify a sample of balances
ERRORS=0
python3 -c "
import json, subprocess, sys

with open('$SNAPSHOT') as f:
    snap = json.load(f)

# Check first 10 holders
for holder in snap['holders'][:10]:
    wallet = holder['wallet']
    for sym, pos in holder['positions'].items():
        expected = int(pos['raw_wei'])
        addr = pos['token_address']
        # Use cast to check balance
        try:
            result = subprocess.run(
                ['cast', 'call', '--rpc-url', '$RPC', addr, 'balanceOf(address)(uint256)', wallet],
                capture_output=True, text=True, timeout=10
            )
            actual = int(result.stdout.strip() or '0')
            status = 'OK' if actual == expected else f'MISMATCH (got {actual})'
            print(f'  {sym} {wallet[:10]}...: {status}')
            if actual != expected:
                sys.exit(1)
        except Exception as e:
            print(f'  {sym} {wallet[:10]}...: ERROR ({e})')
" || ERRORS=1

kill "$PF_PID" 2>/dev/null || true

if [[ "$ERRORS" -eq 0 ]]; then
  echo ""
  echo "=== Chain rebuild complete ==="
  echo "  Environment: $ENV"
  echo "  Chain ID:    $CHAIN_ID"
  echo "  Snapshot:    $(basename "$SNAPSHOT")"
else
  echo ""
  echo "=== VERIFICATION FAILED ==="
  echo "  Some balances do not match. Check the output above."
  exit 1
fi
