#!/usr/bin/env bash
set -euo pipefail

# Bootstrap Liquidity L2 on Lux devnet
# Requires funded mnemonic on P/X chains

export MNEMONIC="${MNEMONIC:?Set MNEMONIC env var}"
export LUX_URI="${LUX_URI:-http://node-0.node-headless.devnet.svc.cluster.local:9631}"
export NETWORK_NAME="Liquidity"
export CHAINS="evm,dex,fhe"
export KEY_INDEX=0

echo "Bootstrapping Liquidity L2 on devnet..."
echo "  URI: $LUX_URI"

lqd bootstrap
