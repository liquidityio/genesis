#!/usr/bin/env bash
set -euo pipefail

export LIGHT_MNEMONIC="${LIGHT_MNEMONIC:-light light light light light light light light light light light energy}"
export MNEMONIC="$LIGHT_MNEMONIC"
export NETWORK_URI="${NETWORK_URI:-http://127.0.0.1:9650}"
export LUX_URI="$NETWORK_URI"
export NETWORK_NAME="Liquidity"
export CHAINS="evm,dex,fhe"
export KEY_INDEX=0

echo "Bootstrapping Liquidity L2 on localnet..."
echo "  URI: $NETWORK_URI"

lqd bootstrap
