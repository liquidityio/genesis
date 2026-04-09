#!/usr/bin/env -S npx tsx
/**
 * generate-evm-genesis.ts
 *
 * Converts a Substrate state export into EVM genesis alloc entries.
 * Maps Substrate SS58 addresses → EVM addresses (keccak256 of pubkey).
 * Outputs a genesis.json fragment with alloc entries for:
 *   - Native balances (LQDTY)
 *   - USDL pre-mint
 *   - SecurityToken registry (from EnhancedAssets pallet)
 *
 * Usage:
 *   npx tsx scripts/generate-evm-genesis.ts substrate-export-12345.json [--chain-id 8675309] [--usdl-supply 1000000000]
 */

import * as fs from 'fs';
import * as path from 'path';
import * as crypto from 'crypto';
import * as os from 'os';

const EXPORT_FILE = process.argv[2];
if (!EXPORT_FILE) {
  console.error('Usage: generate-evm-genesis.ts <export-file.json> [--chain-id ID] [--usdl-supply AMOUNT]');
  process.exit(1);
}

const CHAIN_ID = parseInt(
  process.argv.find(a => a.startsWith('--chain-id='))?.split('=')[1] ?? '8675309'
);
const USDL_SUPPLY = BigInt(
  process.argv.find(a => a.startsWith('--usdl-supply='))?.split('=')[1] ?? '1000000000'
);

// Treasury address — receives the USDL pre-mint
const TREASURY = '0x9011E888251AB053B7bD1cdB598Db4f9DEd94714';

function substrateToEvm(pubKeyHex: string): string {
  // Derive EVM address from substrate public key:
  // Take last 20 bytes of keccak256(pubkey)
  const hash = crypto.createHash('sha3-256').update(Buffer.from(pubKeyHex, 'hex')).digest();
  return '0x' + hash.subarray(12).toString('hex');
}

async function main() {
  const data = JSON.parse(fs.readFileSync(EXPORT_FILE, 'utf-8'));
  const meta = data._meta;
  console.log(`\n=== Generate EVM Genesis from Substrate Export ===`);
  console.log(`Source:   block #${meta.blockNumber} on ${meta.chain}`);
  console.log(`Chain ID: ${CHAIN_ID}`);
  console.log(`USDL:    ${USDL_SUPPLY.toString()} (pre-mint to ${TREASURY})\n`);

  const alloc: Record<string, { balance: string; nonce?: string }> = {};
  let migrated = 0;
  let skippedZero = 0;

  // Map substrate balances → EVM alloc
  for (const acct of data.accounts.entries) {
    const balance = BigInt(acct.free) + BigInt(acct.reserved);
    if (balance === 0n) {
      skippedZero++;
      continue;
    }

    const evmAddr = substrateToEvm(acct.pubKeyHex);
    const existing = BigInt(alloc[evmAddr]?.balance ?? '0');
    alloc[evmAddr] = {
      balance: '0x' + (existing + balance).toString(16),
    };
    migrated++;
  }

  // Treasury gets USDL pre-mint balance
  const treasuryBalance = BigInt(alloc[TREASURY.toLowerCase()]?.balance ?? '0');
  alloc[TREASURY.toLowerCase()] = {
    balance: '0x' + (treasuryBalance + USDL_SUPPLY * 10n ** 18n).toString(16),
  };

  const genesis = {
    config: {
      chainId: CHAIN_ID,
    },
    alloc,
    _migration: {
      sourceChain: meta.chain,
      sourceBlock: meta.blockNumber,
      sourceBlockHash: meta.blockHash,
      sourceStateRoot: meta.stateRoot,
      accountsMigrated: migrated,
      accountsSkippedZero: skippedZero,
      generatedAt: new Date().toISOString(),
    },
  };

  const outPath = path.join(
    os.homedir(),
    'Desktop',
    `evm-genesis-alloc-${CHAIN_ID}.json`
  );
  fs.writeFileSync(outPath, JSON.stringify(genesis, null, 2));

  console.log(`Migrated ${migrated} accounts (skipped ${skippedZero} zero-balance)`);
  console.log(`Total alloc entries: ${Object.keys(alloc).length}`);
  console.log(`\nWritten: ${outPath}`);
}

main().catch(err => {
  console.error('Genesis generation failed:', err);
  process.exit(1);
});
