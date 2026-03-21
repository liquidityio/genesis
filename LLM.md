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

## Related Repos

| Repo | Purpose |
|------|---------|
| `~/work/liquidity/node/` | LQDTY EVM node binary |
| `~/work/liquidity/contracts/` | Solidity contracts (SubstrateImporter, SubstrateMigration) |
