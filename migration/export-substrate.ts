#!/usr/bin/env -S npx tsx
/**
 * export-substrate.ts
 *
 * Full state export from the LQDTY Substrate solochain.
 * Connects via WebSocket RPC, enumerates all storage keys for each pallet,
 * and dumps raw + decoded data to JSON files on ~/Desktop.
 *
 * Pallets exported (runtime index):
 *   4  — System.Account (native balances)
 *  10  — Tokens (pallet_assets)
 *  15  — CommercialLoan (loan lifecycle, NFT certs, repayments)
 *  16  — EnhancedAssets (security tokens, balances, whitelists)
 *  17  — EnhancedMultisig (multisig wallets, intents, roles)
 *  18  — TokenSwap (swap orders)
 *  20  — NFT (token ownership, metadata)
 *  21  — Sponsor (fee delegation tiers, stats)
 *
 * Output: ~/Desktop/substrate-export-{blockNumber}.json
 *
 * Usage:
 *   npx tsx scripts/export-substrate.ts [--rpc wss://mainnet.liquidity.io] [--block latest] [--out ~/Desktop]
 */

import * as fs from 'fs';
import * as path from 'path';
import * as os from 'os';
import { WebSocket } from 'ws';

// ----------------------------------------------------------------
//  CLI args
// ----------------------------------------------------------------

const RPC_URL = getArg('--rpc') ?? process.env.SUBSTRATE_RPC ?? 'ws://127.0.0.1:9944';
const TARGET_BLOCK = getArg('--block') ?? 'latest';
const OUT_DIR = getArg('--out') ?? path.join(os.homedir(), 'Desktop');

function getArg(flag: string): string | undefined {
  const arg = process.argv.find(a => a.startsWith(flag + '='));
  return arg?.split('=').slice(1).join('=');
}

// ----------------------------------------------------------------
//  WebSocket JSON-RPC transport
// ----------------------------------------------------------------

let ws: WebSocket;
let rpcId = 0;
const pending = new Map<number, { resolve: Function; reject: Function }>();

async function connectWs(url: string): Promise<void> {
  return new Promise((resolve, reject) => {
    ws = new WebSocket(url);
    ws.on('open', () => resolve());
    ws.on('error', (err: Error) => reject(err));
    ws.on('close', () => console.log('WebSocket closed'));
    ws.on('message', (data: Buffer) => {
      const msg = JSON.parse(data.toString());
      const p = pending.get(msg.id);
      if (p) {
        pending.delete(msg.id);
        if (msg.error) p.reject(new Error(msg.error.message));
        else p.resolve(msg.result);
      }
    });
  });
}

async function rpc(method: string, params: any[] = []): Promise<any> {
  const id = ++rpcId;
  return new Promise((resolve, reject) => {
    pending.set(id, { resolve, reject });
    ws.send(JSON.stringify({ jsonrpc: '2.0', id, method, params }));
  });
}

// ----------------------------------------------------------------
//  Storage key constants (twox128 hashes of pallet + storage names)
//  Pre-computed from the runtime metadata.
// ----------------------------------------------------------------

// System.Account — all native balances
// twox128("System") + twox128("Account")
const SYSTEM_ACCOUNT_PREFIX = '0x26aa394eea5630e07c48ae0c9558cef7b99d880ec681799c0cf30e8886371da9';

// EnhancedAssets pallet storage prefixes
// We'll enumerate via state_getKeysPaged with runtime metadata
// For now, use metadata-based discovery

// ----------------------------------------------------------------
//  Paginated key enumeration
// ----------------------------------------------------------------

async function getAllKeys(prefix: string, blockHash: string): Promise<string[]> {
  const allKeys: string[] = [];
  let startKey = prefix;
  const PAGE_SIZE = 1000;

  while (true) {
    const keys: string[] = await rpc('state_getKeysPaged', [prefix, PAGE_SIZE, startKey, blockHash]);
    if (!keys || keys.length === 0) break;
    allKeys.push(...keys);
    if (keys.length < PAGE_SIZE) break;
    startKey = keys[keys.length - 1];
  }

  return allKeys;
}

async function getStorageMulti(keys: string[], blockHash: string): Promise<(string | null)[]> {
  // Batch in groups of 200 to avoid overwhelming the node
  const BATCH = 200;
  const results: (string | null)[] = [];

  for (let i = 0; i < keys.length; i += BATCH) {
    const batch = keys.slice(i, i + BATCH);
    const values = await rpc('state_queryStorageAt', [batch, blockHash]);
    if (values && values.length > 0 && values[0].changes) {
      const changeMap = new Map<string, string | null>();
      for (const [k, v] of values[0].changes) {
        changeMap.set(k, v);
      }
      for (const key of batch) {
        results.push(changeMap.get(key) ?? null);
      }
    } else {
      // Fallback: query individually
      for (const key of batch) {
        const val = await rpc('state_getStorage', [key, blockHash]);
        results.push(val ?? null);
      }
    }
  }

  return results;
}

// ----------------------------------------------------------------
//  Utility: SCALE / hex decoding helpers
// ----------------------------------------------------------------

function leHexToDecimal(hex: string): string {
  if (!hex || hex === '0x' || hex === '') return '0';
  const clean = hex.replace('0x', '');
  const bytes = clean.match(/.{2}/g)?.reverse().join('') ?? '0';
  return BigInt('0x' + bytes).toString();
}

function leHexToU32(hex: string): number {
  return Number(leHexToDecimal(hex.slice(0, 8)));
}

function leHexToU64(hex: string): string {
  return leHexToDecimal(hex.slice(0, 16));
}

function leHexToU128(hex: string): string {
  return leHexToDecimal(hex.slice(0, 32));
}

function hexToUtf8(hex: string): string {
  const bytes = Buffer.from(hex, 'hex');
  return bytes.toString('utf8').replace(/\0/g, '');
}

function decodeBoundedVec(hex: string): { data: string; bytesConsumed: number } {
  // SCALE compact length prefix
  const firstByte = parseInt(hex.slice(0, 2), 16);
  let len: number;
  let offset: number;

  if ((firstByte & 0x03) === 0) {
    len = firstByte >> 2;
    offset = 2;
  } else if ((firstByte & 0x03) === 1) {
    len = (parseInt(hex.slice(0, 4).match(/.{2}/g)!.reverse().join(''), 16)) >> 2;
    offset = 4;
  } else if ((firstByte & 0x03) === 2) {
    len = (parseInt(hex.slice(0, 8).match(/.{2}/g)!.reverse().join(''), 16)) >> 2;
    offset = 8;
  } else {
    // Big integer mode — unlikely for our data
    len = 0;
    offset = 2;
  }

  const data = hex.slice(offset, offset + len * 2);
  return { data, bytesConsumed: offset + len * 2 };
}

// ----------------------------------------------------------------
//  Metadata-driven storage prefix computation
// ----------------------------------------------------------------

async function getMetadata(blockHash: string) {
  const raw = await rpc('state_getMetadata', [blockHash]);
  return raw; // hex-encoded metadata blob
}

// Compute twox128 of a string — we query the node for the prefix
// instead of implementing xxhash locally
async function getStoragePrefix(pallet: string, storage: string, blockHash: string): Promise<string> {
  // state_getKeysPaged with a known prefix works even without computing twox128 ourselves
  // Instead, use the metadata to figure out the prefix, or just enumerate
  // For well-known pallets, use the standard approach:
  // twox128(pallet) ++ twox128(storage)
  // We'll compute these via the node's state_call or just try known prefixes

  // Actually, let's just enumerate ALL storage and group by prefix
  // Or use the runtime metadata...

  // Simpler: query state_getKeys with empty prefix and filter
  // But that could be huge. Better to use metadata.

  // For now, return empty — we'll use the full enumeration approach
  return '';
}

// ----------------------------------------------------------------
//  Full state dump — enumerate ALL storage
// ----------------------------------------------------------------

interface RawStorageEntry {
  key: string;
  value: string;
}

interface PalletExport {
  name: string;
  storageCount: number;
  entries: RawStorageEntry[];
}

async function exportFullState(blockHash: string): Promise<{
  pallets: Record<string, PalletExport>;
  totalKeys: number;
}> {
  console.log('  Enumerating ALL storage keys (this may take a while)...');

  const allKeys = await getAllKeys('0x', blockHash);
  console.log(`  Total storage keys: ${allKeys.length}`);

  // Group keys by their first 32 hex chars (16 bytes = pallet prefix)
  const byPrefix = new Map<string, string[]>();
  for (const key of allKeys) {
    const prefix = key.slice(0, 34); // 0x + 32 hex chars
    const group = byPrefix.get(prefix) ?? [];
    group.push(key);
    byPrefix.set(prefix, group);
  }

  console.log(`  Found ${byPrefix.size} distinct pallet prefixes`);

  // Fetch all values
  console.log('  Fetching all storage values...');
  const allValues = await getStorageMulti(allKeys, blockHash);

  // Build the export
  const pallets: Record<string, PalletExport> = {};
  let keyIdx = 0;

  for (const key of allKeys) {
    const palletPrefix = key.slice(0, 34);
    const storagePrefix = key.slice(0, 66); // pallet (32) + storage (32) = 64 hex + 0x

    if (!pallets[palletPrefix]) {
      pallets[palletPrefix] = {
        name: `prefix_${palletPrefix.slice(2, 10)}`,
        storageCount: 0,
        entries: [],
      };
    }

    const value = allValues[keyIdx];
    if (value) {
      pallets[palletPrefix].entries.push({
        key,
        value,
      });
      pallets[palletPrefix].storageCount++;
    }
    keyIdx++;
  }

  return { pallets, totalKeys: allKeys.length };
}

// ----------------------------------------------------------------
//  Decoded exports for known pallets
// ----------------------------------------------------------------

interface AccountExport {
  storageKey: string;
  pubKeyHex: string;
  nonce: number;
  free: string;
  reserved: string;
  frozen: string;
}

async function exportAccounts(blockHash: string): Promise<AccountExport[]> {
  console.log('  Exporting System.Account balances...');

  const keys = await getAllKeys(SYSTEM_ACCOUNT_PREFIX, blockHash);
  console.log(`  Found ${keys.length} accounts`);
  if (keys.length === 0) return [];

  const values = await getStorageMulti(keys, blockHash);
  const accounts: AccountExport[] = [];

  for (let i = 0; i < keys.length; i++) {
    const value = values[i];
    if (!value) continue;

    const keyHex = keys[i].replace('0x', '');
    // Storage key layout: 32 bytes prefix + 16 bytes blake2b128 hash + 32 bytes pubkey
    const pubKey = keyHex.slice(96); // last 32 bytes (64 hex chars)

    const dataHex = value.replace('0x', '');
    // AccountInfo: nonce(4 LE) + consumers(4) + providers(4) + sufficients(4) = 16 bytes = 32 hex
    // AccountData: free(16 LE) + reserved(16 LE) + frozen(16 LE) + flags(16 LE)
    const nonce = leHexToU32(dataHex.slice(0, 8));
    const free = leHexToU128(dataHex.slice(32, 64));
    const reserved = leHexToU128(dataHex.slice(64, 96));
    const frozen = leHexToU128(dataHex.slice(96, 128));

    accounts.push({
      storageKey: keys[i],
      pubKeyHex: pubKey,
      nonce,
      free,
      reserved,
      frozen,
    });
  }

  console.log(`  Exported ${accounts.length} accounts (${accounts.filter(a => a.free !== '0').length} with free balance)`);
  return accounts;
}

// ----------------------------------------------------------------
//  Export pallet by raw key prefix
// ----------------------------------------------------------------

async function exportPalletRaw(prefix: string, name: string, blockHash: string): Promise<RawStorageEntry[]> {
  console.log(`  Exporting ${name}...`);
  const keys = await getAllKeys(prefix, blockHash);
  console.log(`  Found ${keys.length} ${name} keys`);

  if (keys.length === 0) return [];

  const values = await getStorageMulti(keys, blockHash);
  const entries: RawStorageEntry[] = [];

  for (let i = 0; i < keys.length; i++) {
    if (values[i]) {
      entries.push({ key: keys[i], value: values[i]! });
    }
  }

  return entries;
}

// ----------------------------------------------------------------
//  Main
// ----------------------------------------------------------------

async function main() {
  console.log(`\n=== LQDTY Substrate Full State Export ===`);
  console.log(`RPC:    ${RPC_URL}`);
  console.log(`Block:  ${TARGET_BLOCK}`);
  console.log(`Output: ${OUT_DIR}\n`);

  console.log(`Connecting to ${RPC_URL}...`);
  await connectWs(RPC_URL);

  const chain = await rpc('system_chain');
  const version = await rpc('system_version');
  const runtime = await rpc('state_getRuntimeVersion', []);
  console.log(`Connected: ${chain} (${version})`);
  console.log(`Runtime: ${runtime?.specName ?? 'unknown'} v${runtime?.specVersion ?? '?'}\n`);

  // Get finalized block
  let blockHash: string;
  if (TARGET_BLOCK === 'latest') {
    blockHash = await rpc('chain_getFinalizedHead');
  } else {
    blockHash = await rpc('chain_getBlockHash', [parseInt(TARGET_BLOCK)]);
  }

  const header = await rpc('chain_getHeader', [blockHash]);
  const blockNumber = parseInt(header.number, 16);
  console.log(`Exporting state at block #${blockNumber} (${blockHash})\n`);

  // Get raw metadata for reference
  const metadataHex = await getMetadata(blockHash);

  // ---- Export each pallet ----

  // 1. System.Account (native balances)
  const accounts = await exportAccounts(blockHash);

  // 2. Full raw state dump (all pallets, all storage)
  const fullState = await exportFullState(blockHash);

  // ---- Assemble output ----

  const exportData = {
    _meta: {
      chain,
      version,
      runtimeSpec: runtime?.specName,
      runtimeVersion: runtime?.specVersion,
      blockNumber,
      blockHash,
      stateRoot: header.stateRoot,
      parentHash: header.parentHash,
      exportedAt: new Date().toISOString(),
      rpcEndpoint: RPC_URL,
    },
    accounts: {
      count: accounts.length,
      totalFree: accounts.reduce((s, a) => s + BigInt(a.free), 0n).toString(),
      totalReserved: accounts.reduce((s, a) => s + BigInt(a.reserved), 0n).toString(),
      entries: accounts,
    },
    rawState: {
      totalKeys: fullState.totalKeys,
      palletPrefixes: Object.keys(fullState.pallets).length,
      pallets: Object.fromEntries(
        Object.entries(fullState.pallets).map(([prefix, pallet]) => [
          prefix,
          {
            name: pallet.name,
            entryCount: pallet.entries.length,
            entries: pallet.entries,
          },
        ])
      ),
    },
    metadata: metadataHex, // Full runtime metadata for offline decoding
  };

  // Write main export
  const filename = `substrate-export-${blockNumber}.json`;
  const filepath = path.join(OUT_DIR, filename);
  fs.writeFileSync(filepath, JSON.stringify(exportData, null, 2));

  // Write a compact summary (no raw entries, no metadata)
  const summary = {
    ...exportData._meta,
    accounts: {
      count: accounts.length,
      withBalance: accounts.filter(a => a.free !== '0' || a.reserved !== '0').length,
      totalFree: exportData.accounts.totalFree,
      totalReserved: exportData.accounts.totalReserved,
    },
    storage: {
      totalKeys: fullState.totalKeys,
      palletPrefixes: Object.keys(fullState.pallets).length,
      breakdown: Object.fromEntries(
        Object.entries(fullState.pallets).map(([prefix, p]) => [prefix, p.entries.length])
      ),
    },
  };

  const summaryPath = path.join(OUT_DIR, `substrate-summary-${blockNumber}.json`);
  fs.writeFileSync(summaryPath, JSON.stringify(summary, null, 2));

  // ---- Print summary ----
  console.log(`\n=== Export Complete ===`);
  console.log(`Block:        #${blockNumber}`);
  console.log(`State root:   ${header.stateRoot}`);
  console.log(`Accounts:     ${accounts.length} (${accounts.filter(a => a.free !== '0').length} with balance)`);
  console.log(`Total free:   ${exportData.accounts.totalFree}`);
  console.log(`Total keys:   ${fullState.totalKeys}`);
  console.log(`Pallet groups: ${Object.keys(fullState.pallets).length}`);
  console.log(`\nFiles written:`);
  console.log(`  ${filepath}`);
  console.log(`  ${summaryPath}`);

  ws.close();
}

main().catch(err => {
  console.error('Export failed:', err);
  process.exit(1);
});
