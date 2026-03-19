# LQDTY Genesis

Substrate mainnet state export and EVM L1 import data for the LQDTY chain migration.

## Files

| File | Description |
|------|-------------|
| `lqdty-mainnet-export.json` | Raw Substrate state at block 6,491,829 |
| `lqdty-evm-import.json` | Redenominated + batched for `SubstrateImporter.sol` |

## Redenomination

```
Substrate:  10,000,000,000,000,000 LQDTY @ 12 decimals
EVM L1:     10,000,000,000 LQDTY @ 18 decimals
Ratio:      1:1,000,000 (proportional, zero drift)
```

## Chain IDs

| Network | Chain ID |
|---------|----------|
| Mainnet | 8675309 |
| Testnet | 8675310 |

## Data Summary

- **1,727 accounts** with non-zero balances
- **8 loan records** from karus_loan pallet
- **No PII on-chain** (VINs masked, no SSN/DOB/names)

### Top Holders (post-redenomination)

| Account | Balance | Share |
|---------|---------|-------|
| Treasury | 8,499,999,953 LQDTY | 85% |
| Distribution | 999,992,794 LQDTY | 10% |
| Reserve | 500,000,000 LQDTY | 5% |

## Scripts

### Export from Substrate

Connects to the live Substrate node via GKE pod and dumps all state:

```bash
python3 scripts/export.py
# Requires: gcloud auth + kubectl access to apps-gke (us-central1)
# Output: lqdty-mainnet-export.json
```

### Redenominate

Converts Substrate export (12 dec) to EVM import format (18 dec):

```bash
python3 scripts/redenominate.py
# Input:  lqdty-mainnet-export.json
# Output: lqdty-evm-import.json
```

### Import to EVM L1

Import is done via [liquidityio/contracts](https://github.com/liquidityio/contracts):

```bash
cd ../contracts

# Deploy all contracts + SubstrateImporter
forge script script/Deploy.s.sol --rpc-url $RPC --broadcast

# Import batches (see script/Import.s.sol)
for i in $(seq 1 35); do
  BATCH_ID=$i IMPORTER=$ADDR forge script script/Import.s.sol --rpc-url $RPC --broadcast
done

# Finalize
FINALIZE=1 IMPORTER=$ADDR forge script script/Import.s.sol --rpc-url $RPC --broadcast
```

## Import Flow

```
1. Export Substrate state     → lqdty-mainnet-export.json
2. Redenominate (1:1M ratio)  → lqdty-evm-import.json
3. Deploy LQDTY EVM L1        → chain ID 8675309
4. Deploy SubstrateImporter   → grants mint rights
5. importBalances() × 35      → 1,727 accounts
6. importLoans() × 1          → 8 loan records
7. finalizeMigration()        → permanent lock
8. Users claim via SR25519    → SubstrateMigration.sol
```

## Repos

| Repo | Purpose |
|------|---------|
| [liquidityio/genesis](https://github.com/liquidityio/genesis) | This repo — state export + import data |
| [liquidityio/node](https://github.com/liquidityio/node) | LQDTY EVM L1 node (chain ID 8675309) |
| [liquidityio/contracts](https://github.com/liquidityio/contracts) | Solidity contracts (tokens, loans, migration) |
