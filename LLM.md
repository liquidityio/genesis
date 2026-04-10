# Liquidity Genesis — AI Engineering Guide

## Overview

Substrate mainnet state export and EVM L2 import data for migrating the LQDTY token from Substrate to the Liquidity EVM on Lux Network.

## Redenomination

```
Substrate:  10,000,000,000,000,000 LQDTY @ 12 decimals
EVM L2:     10,000,000,000 LQDTY @ 18 decimals
Ratio:      1:1,000,000 (proportional, zero drift)
```

## Files

| File | Description |
|------|-------------|
| `lqdty-mainnet-export.json` | Raw Substrate state at block 6,491,829 (518KB) |
| `lqdty-evm-import.json` | Redenominated + batched for SubstrateImporter.sol (182KB) |
| `scripts/export.py` | Export from live Substrate node (requires GKE access) |
| `scripts/redenominate.py` | Convert 12-dec → 18-dec for EVM import |

## Data Summary

- **1,727 accounts** with non-zero balances
- **8 loan records** from karus_loan pallet
- **No PII on-chain** (VINs masked)

### Top Holders (post-redenomination)

| Account | Balance | Share |
|---------|---------|-------|
| Treasury | 8,499,999,953 LQDTY | 85% |
| Distribution | 999,992,794 LQDTY | 10% |
| Reserve | 500,000,000 LQDTY | 5% |

## Migration Flow

```
1. Export Substrate state     → lqdty-mainnet-export.json
2. Redenominate (1:1M ratio)  → lqdty-evm-import.json
3. Deploy LQDTY EVM L2        → Liquidity subnet on Lux
4. Deploy SubstrateImporter   → grants mint rights
5. importBalances() × 35      → 1,727 accounts
6. importLoans() × 1          → 8 loan records
7. finalizeMigration()        → permanent lock
8. Users claim via SR25519    → SubstrateMigration.sol precompile
```

The SR25519 precompile on the LQDTY EVM allows Substrate users to prove ownership of their old account and claim tokens on the new EVM chain.

## Target Economics

Post-migration, the Liquidity EVM should hold:
- **10M LQDTY** circulating ($100K at $0.01/LQDTY)
- **10B LQDTY** total supply (genesis allocation to treasury)
- Treasury retains the difference for future distribution

## Chain IDs

| Network | Chain ID |
|---------|----------|
| Mainnet | 8675309 |
| Testnet | 8675310 |
| Devnet | 8675311 |

## Genesis Alloc Generator (2026-04-09)

`cmd/genesis-alloc/` generates a Lux EVM genesis alloc JSON from an Avalanche snapshot.
Each SecurityToken gets a deterministic address, every holder's balanceOf is pre-set in
storage slots, totalSupply matches sum of all balances, and deployer gets MINTER_ROLE +
DEFAULT_ADMIN_ROLE via OZ 5.x ERC-7201 AccessControl storage layout.

```bash
go run ./cmd/genesis-alloc/ \
  --snapshot ~/work/liquidity/state/chain/snapshots/SNAPSHOT_82552152.json \
  --output genesis-alloc.json \
  --chain-id 8675309 \
  --manifest ~/work/liquidity/contracts/deployments/mainnet-20260327.json \
  --horse-manifest ~/work/liquidity/contracts/deployments/horse-deploy-mainnet-1775784137.json
```

Snapshot format: 90 holders, 26 tokens, 521 positions, $70,794 USDL total.

### ERC20 Storage Layout (OZ 5.x non-upgradeable)

- Slot 0: `_balances` mapping
- Slot 2: `_totalSupply`
- `balanceOf[addr]` = `keccak256(abi.encode(addr, uint256(0)))`
- AccessControl base = `keccak256("openzeppelin.storage.AccessControl") - 1`

### Token Addresses

Uses existing manifest addresses from deployments/. Tokens not in any manifest
get a deterministic CREATE2 address (salt = keccak256(symbol)).

## Hourly Mint (2026-04-09)

`cmd/hourly-mint/` diffs two snapshots and mints only new/increased positions.
Uses `cast send` for signing. Idempotent (checks balanceOf before minting).

```bash
go run ./cmd/hourly-mint/ \
  --previous SNAPSHOT_OLD.json \
  --current SNAPSHOT_NEW.json \
  --rpc http://... \
  --private-key $KEY \
  --dry-run  # safe preview
```

Rules: monotonically increasing only. Decreased positions = no action.

## Chain Rebuild Script

`scripts/rebuild-chain.sh` orchestrates a full chain rebuild:
1. Build genesis-alloc
2. Extract bytecodes from existing chain (via cast)
3. Generate alloc from snapshot
4. Merge into evm.json
5. Stop nodes, delete PVCs, update ConfigMap
6. Restart nodes, verify balances

```bash
./scripts/rebuild-chain.sh --env mainnet --snapshot /path/to/SNAPSHOT.json --dry-run
```

## Related Repos

| Repo | Purpose |
|------|---------|
| `~/work/liquidity/node/` | LQDTY EVM node binary |
| `~/work/liquidity/contracts/` | Solidity contracts (SubstrateImporter, SubstrateMigration) |
| `~/work/liquidity/state/` | Production state snapshots + hourly workflow |
