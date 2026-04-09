#!/usr/bin/env python3
"""
redenominate.py — Convert LQDTY Substrate export to EVM-ready import batches.

Reads:  lqdty-mainnet-export.json (from export-substrate.ts)
Writes: ~/Desktop/lqdty-evm-import.json

Redenomination:
  Old: 10 quadrillion LQDTY @ 12 decimals (Substrate)
  New: 10 billion LQDTY @ 18 decimals (EVM)
  Ratio: 1e-6 (every old balance divided by 1,000,000)

Output format matches SubstrateImporter.sol batch import functions.
"""

import json
import os
import sys
from decimal import Decimal, getcontext

getcontext().prec = 50  # high precision for large numbers

# --- Config ---
OLD_DECIMALS = 12
NEW_DECIMALS = 18
OLD_SUPPLY = 10_000_000_000_000_000  # 10Q
NEW_SUPPLY = 10_000_000_000          # 10B
RATIO = Decimal(NEW_SUPPLY) / Decimal(OLD_SUPPLY)  # 1e-6
BATCH_SIZE = 50  # accounts per import batch

SCRIPT_DIR = os.path.dirname(os.path.abspath(__file__))
REPO_DIR = os.path.dirname(SCRIPT_DIR)
INPUT = os.path.join(REPO_DIR, "lqdty-mainnet-export.json")
OUTPUT = os.path.join(REPO_DIR, "lqdty-evm-import.json")

def redenominate(old_raw: str) -> str:
    """Convert old raw balance (12 dec) to new raw balance (18 dec)."""
    old = Decimal(old_raw)
    # old_human = old / 10^12
    # new_human = old_human * ratio
    # new_raw = new_human * 10^18
    # = (old / 10^12) * (10B / 10Q) * 10^18
    # = old * 10^-6 * 10^6
    # = old * 1  (the factors cancel!)
    # But we want exact integer arithmetic:
    new_raw = (old * NEW_SUPPLY * (10 ** NEW_DECIMALS)) // (OLD_SUPPLY * (10 ** OLD_DECIMALS))
    return str(int(new_raw))

def main():
    print(f"Reading {INPUT}...")
    with open(INPUT) as f:
        data = json.load(f)

    accounts = data["accounts"]
    loans = data.get("loans", [])

    print(f"Accounts: {len(accounts)}")
    print(f"Loans: {len(loans)}")
    print(f"Redenomination: {OLD_SUPPLY:,} → {NEW_SUPPLY:,} LQDTY")
    print(f"Decimals: {OLD_DECIMALS} → {NEW_DECIMALS}")
    print(f"Ratio: {float(RATIO)}")
    print()

    # --- Redenominate balances ---
    imports = []
    total_old = Decimal(0)
    total_new = Decimal(0)
    zero_balance = 0

    for acc in accounts:
        old_free = acc["f"]
        old_reserved = acc["r"]
        new_free = redenominate(old_free)
        new_reserved = redenominate(old_reserved)

        total_old += Decimal(old_free) + Decimal(old_reserved)
        total_new += Decimal(new_free) + Decimal(new_reserved)

        if new_free == "0" and new_reserved == "0":
            zero_balance += 1
            continue

        imports.append({
            "substratePubKey": "0x" + acc["p"],
            "evmAddress": "",  # to be mapped during migration
            "freeBalance": new_free,
            "reservedBalance": new_reserved,
            "old_free": old_free,
            "old_reserved": old_reserved,
        })

    # Sort by balance descending
    imports.sort(key=lambda x: int(x["freeBalance"]), reverse=True)

    # --- Split into batches ---
    batches = []
    for i in range(0, len(imports), BATCH_SIZE):
        batch = imports[i:i + BATCH_SIZE]
        batches.append({
            "batchId": len(batches) + 1,
            "count": len(batch),
            "imports": batch,
        })

    # --- Loan records (pass through, no balance redenomination needed) ---
    loan_imports = []
    for loan in loans:
        loan_imports.append({
            "storageKey": loan["key"],
            "asciiPreview": loan["ascii"][:500],
            "rawLength": loan["len"],
        })

    # --- Summary ---
    old_human = total_old / Decimal(10 ** OLD_DECIMALS)
    new_human = total_new / Decimal(10 ** NEW_DECIMALS)

    print(f"Old total: {old_human:,.4f} LQDTY ({OLD_DECIMALS} dec)")
    print(f"New total: {new_human:,.4f} LQDTY ({NEW_DECIMALS} dec)")
    print(f"Accounts with balance: {len(imports)}")
    print(f"Zero-balance accounts skipped: {zero_balance}")
    print(f"Batches: {len(batches)} (batch size: {BATCH_SIZE})")
    print(f"Loan records: {len(loan_imports)}")
    print()

    # --- Top 10 ---
    print("Top 10 balances after redenomination:")
    for acc in imports[:10]:
        bal = Decimal(acc["freeBalance"]) / Decimal(10 ** NEW_DECIMALS)
        print(f"  {acc['substratePubKey'][:20]}... {bal:>20,.4f} LQDTY")
    print()

    # --- Verify ---
    expected_ratio = Decimal(NEW_SUPPLY) / Decimal(OLD_SUPPLY)
    actual_ratio = new_human / old_human if old_human > 0 else Decimal(0)
    print(f"Expected ratio: {float(expected_ratio):.10f}")
    print(f"Actual ratio:   {float(actual_ratio):.10f}")
    drift = abs(float(actual_ratio - expected_ratio))
    print(f"Drift:          {drift:.15f} {'✓ OK' if drift < 1e-10 else '*** MISMATCH ***'}")

    # --- Write output ---
    output = {
        "chain": "LQDTY",
        "migrationBlock": data.get("block", "latest"),
        "timestamp": data.get("timestamp", ""),
        "redenomination": {
            "oldSupply": str(OLD_SUPPLY),
            "newSupply": str(NEW_SUPPLY),
            "oldDecimals": OLD_DECIMALS,
            "newDecimals": NEW_DECIMALS,
            "ratio": str(float(RATIO)),
        },
        "summary": {
            "accountsWithBalance": len(imports),
            "zeroBalanceSkipped": zero_balance,
            "batchCount": len(batches),
            "loanRecords": len(loan_imports),
            "totalNewSupply": str(int(total_new)),
        },
        "balanceBatches": batches,
        "loans": loan_imports,
    }

    with open(OUTPUT, "w") as f:
        json.dump(output, f, indent=2)

    print(f"\nWritten to {OUTPUT}")
    print(f"File size: {os.path.getsize(OUTPUT):,} bytes")

if __name__ == "__main__":
    main()
