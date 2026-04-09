#!/usr/bin/env bash
set -euo pipefail

export MNEMONIC="${MNEMONIC:?Set MNEMONIC env var}"
export NETWORK_URI="${NETWORK_URI:-http://node-0.node-headless.devnet.svc.cluster.local:9631}"
export LUX_URI="$NETWORK_URI"
export NETWORK_NAME="Liquidity"
export CHAINS="evm,dex,fhe"
export KEY_INDEX=0

echo "Bootstrapping Liquidity L2 on devnet..."
echo "  URI: $NETWORK_URI"

lqd bootstrap
