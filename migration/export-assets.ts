#!/usr/bin/env -S npx tsx
/**
 * export-assets.ts
 *
 * Targeted export of pallet_assets (Tokens, index 10) from the LQDTY Substrate chain.
 * This is where USDL and other fungible tokens live on the old chain.
 *
 * pallet_assets storage layout (AssetId = u32, Balance = u128):
 *   Asset:    StorageMap<AssetId, AssetDetails>
 *   Account:  StorageDoubleMap<AssetId, AccountId, AssetAccount>
 *   Metadata: StorageMap<AssetId, AssetMetadata>
 *
 * Also exports EnhancedAssets (pallet index 16) which holds security tokens.
 *
 * Output: ~/Desktop/substrate-assets-{block}.json
 *
 * Usage:
 *   npx tsx scripts/export-assets.ts [--rpc wss://mainnet.liquidity.io] [--block latest]
 */

import * as fs from 'fs';
import * as path from 'path';
import * as os from 'os';
import { WebSocket } from 'ws';

const RPC_URL = getArg('--rpc') ?? process.env.SUBSTRATE_RPC ?? 'ws://127.0.0.1:9944';
const TARGET_BLOCK = getArg('--block') ?? 'latest';
const OUT_DIR = getArg('--out') ?? path.join(os.homedir(), 'Desktop');

function getArg(flag: string): string | undefined {
  return process.argv.find(a => a.startsWith(flag + '='))?.split('=').slice(1).join('=');
}

let ws: WebSocket;
let rpcId = 0;
const pending = new Map<number, { resolve: Function; reject: Function }>();

async function connectWs(url: string): Promise<void> {
  return new Promise((resolve, reject) => {
    ws = new WebSocket(url);
    ws.on('open', () => resolve());
    ws.on('error', (err: Error) => reject(err));
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

async function getAllKeys(prefix: string, blockHash: string): Promise<string[]> {
  const allKeys: string[] = [];
  let startKey = prefix;
  while (true) {
    const keys: string[] = await rpc('state_getKeysPaged', [prefix, 1000, startKey, blockHash]);
    if (!keys || keys.length === 0) break;
    allKeys.push(...keys);
    if (keys.length < 1000) break;
    startKey = keys[keys.length - 1];
  }
  return allKeys;
}

async function getStorageBatch(keys: string[], blockHash: string): Promise<(string | null)[]> {
  const results: (string | null)[] = [];
  for (let i = 0; i < keys.length; i += 200) {
    const batch = keys.slice(i, i + 200);
    const values = await rpc('state_queryStorageAt', [batch, blockHash]);
    if (values?.[0]?.changes) {
      const map = new Map(values[0].changes.map((c: string[]) => [c[0], c[1]]));
      for (const key of batch) results.push((map.get(key) as string) ?? null);
    } else {
      for (const key of batch) {
        results.push(await rpc('state_getStorage', [key, blockHash]));
      }
    }
  }
  return results;
}

function leHexToDecimal(hex: string, byteLen: number): string {
  if (!hex) return '0';
  const clean = hex.replace('0x', '').slice(0, byteLen * 2);
  const bytes = clean.match(/.{2}/g)?.reverse().join('') ?? '0';
  return BigInt('0x' + bytes).toString();
}

function hexToUtf8(hex: string): string {
  return Buffer.from(hex, 'hex').toString('utf8').replace(/\0/g, '');
}

function decodeCompactLen(hex: string): { len: number; offset: number } {
  const first = parseInt(hex.slice(0, 2), 16);
  if ((first & 3) === 0) return { len: first >> 2, offset: 2 };
  if ((first & 3) === 1) {
    const val = parseInt(hex.slice(0, 4).match(/.{2}/g)!.reverse().join(''), 16);
    return { len: val >> 2, offset: 4 };
  }
  if ((first & 3) === 2) {
    const val = parseInt(hex.slice(0, 8).match(/.{2}/g)!.reverse().join(''), 16);
    return { len: val >> 2, offset: 8 };
  }
  return { len: 0, offset: 2 }; // big int mode, unlikely
}

// ----------------------------------------------------------------
//  pallet_assets storage prefixes (twox128)
//  These are standard frame_support storage key prefixes.
//  twox128("Tokens") is the pallet prefix (named "Tokens" in construct_runtime)
// ----------------------------------------------------------------

// We discover prefixes dynamically by enumerating metadata,
// but also export raw keys grouped by prefix for safety.

async function main() {
  console.log(`\n=== LQDTY Substrate Asset Export ===`);
  console.log(`RPC:    ${RPC_URL}`);
  console.log(`Block:  ${TARGET_BLOCK}\n`);

  await connectWs(RPC_URL);

  const chain = await rpc('system_chain');
  const version = await rpc('system_version');
  console.log(`Connected: ${chain} (${version})`);

  let blockHash: string;
  if (TARGET_BLOCK === 'latest') {
    blockHash = await rpc('chain_getFinalizedHead');
  } else {
    blockHash = await rpc('chain_getBlockHash', [parseInt(TARGET_BLOCK)]);
  }
  const header = await rpc('chain_getHeader', [blockHash]);
  const blockNumber = parseInt(header.number, 16);
  console.log(`Block #${blockNumber} (${blockHash})\n`);

  // ---- Get runtime metadata to discover pallet prefixes ----
  // Instead of computing twox128 locally, enumerate ALL keys and group by prefix
  console.log('Enumerating all storage keys...');
  const allKeys = await getAllKeys('0x', blockHash);
  console.log(`Total keys: ${allKeys.length}`);

  // Group by pallet prefix (first 32 hex chars after 0x = 16 bytes)
  const byPalletPrefix = new Map<string, string[]>();
  for (const key of allKeys) {
    const prefix = key.slice(0, 34); // 0x + 32 hex
    const group = byPalletPrefix.get(prefix) ?? [];
    group.push(key);
    byPalletPrefix.set(prefix, group);
  }

  // Group by storage prefix (first 64 hex chars after 0x = 32 bytes = pallet + storage)
  const byStoragePrefix = new Map<string, string[]>();
  for (const key of allKeys) {
    const prefix = key.length >= 66 ? key.slice(0, 66) : key;
    const group = byStoragePrefix.get(prefix) ?? [];
    group.push(key);
    byStoragePrefix.set(prefix, group);
  }

  console.log(`\nPallet prefixes: ${byPalletPrefix.size}`);
  console.log(`Storage prefixes: ${byStoragePrefix.size}`);
  console.log('\nBreakdown by pallet prefix:');
  for (const [prefix, keys] of [...byPalletPrefix.entries()].sort((a, b) => b[1].length - a[1].length)) {
    console.log(`  ${prefix}: ${keys.length} keys`);
  }

  // ---- Fetch ALL storage values ----
  console.log('\nFetching all storage values...');
  const allValues = await getStorageBatch(allKeys, blockHash);

  // ---- Build raw export ----
  const rawExport: Record<string, { keys: number; entries: Array<{ key: string; value: string }> }> = {};

  for (let i = 0; i < allKeys.length; i++) {
    const palletPrefix = allKeys[i].slice(0, 34);
    if (!rawExport[palletPrefix]) {
      rawExport[palletPrefix] = { keys: 0, entries: [] };
    }
    if (allValues[i]) {
      rawExport[palletPrefix].keys++;
      rawExport[palletPrefix].entries.push({
        key: allKeys[i],
        value: allValues[i]!,
      });
    }
  }

  // ---- Write output ----
  const output = {
    _meta: {
      chain,
      version,
      blockNumber,
      blockHash,
      stateRoot: header.stateRoot,
      exportedAt: new Date().toISOString(),
      totalKeys: allKeys.length,
      totalWithValue: allValues.filter(Boolean).length,
      palletPrefixes: byPalletPrefix.size,
      storagePrefixes: byStoragePrefix.size,
    },
    pallets: rawExport,
  };

  const outPath = path.join(OUT_DIR, `substrate-assets-${blockNumber}.json`);
  fs.writeFileSync(outPath, JSON.stringify(output, null, 2));
  console.log(`\nWritten: ${outPath}`);

  // Also write a human-readable summary
  const summaryLines: string[] = [
    `# Substrate State Export — Block #${blockNumber}`,
    `Chain: ${chain} (${version})`,
    `State root: ${header.stateRoot}`,
    `Exported: ${new Date().toISOString()}`,
    `Total keys: ${allKeys.length}`,
    '',
    '## Pallet Breakdown',
    '',
    '| Prefix | Keys |',
    '|--------|------|',
  ];
  for (const [prefix, keys] of [...byPalletPrefix.entries()].sort((a, b) => b[1].length - a[1].length)) {
    summaryLines.push(`| ${prefix} | ${keys.length} |`);
  }
  summaryLines.push('');
  summaryLines.push('## Storage Breakdown');
  summaryLines.push('');
  summaryLines.push('| Storage Prefix | Keys |');
  summaryLines.push('|----------------|------|');
  for (const [prefix, keys] of [...byStoragePrefix.entries()].sort((a, b) => b[1].length - a[1].length).slice(0, 50)) {
    summaryLines.push(`| ${prefix} | ${keys.length} |`);
  }

  const summaryPath = path.join(OUT_DIR, `substrate-state-summary-${blockNumber}.md`);
  fs.writeFileSync(summaryPath, summaryLines.join('\n'));
  console.log(`Summary: ${summaryPath}`);

  ws.close();
}

main().catch(err => {
  console.error('Export failed:', err);
  process.exit(1);
});
