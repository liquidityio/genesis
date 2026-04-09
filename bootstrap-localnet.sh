#!/usr/bin/env bash
set -euo pipefail

# Bootstrap Liquidity L2 on localnet
# Uses the light mnemonic (standard Anvil/Hardhat test accounts)

export MNEMONIC="${MNEMONIC:-test test test test test test test test test test test junk}"
export LUX_URI="${LUX_URI:-http://127.0.0.1:9650}"
export NETWORK_NAME="Liquidity"
export CHAINS="evm,dex,fhe"
export KEY_INDEX=0

echo "Bootstrapping Liquidity L2 on localnet..."
echo "  URI: $LUX_URI"
echo "  Mnemonic: ${MNEMONIC:0:20}..."

lqd bootstrap
