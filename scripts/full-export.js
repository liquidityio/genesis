#!/usr/bin/env node
/**
 * full-export.js — Complete Substrate state dump.
 *
 * Runs INSIDE a GKE pod (chain service) via kubectl exec.
 * Enumerates ALL 12,500+ storage keys, fetches every value,
 * groups by pallet prefix, and outputs JSON to stdout.
 *
 * Usage (from local machine):
 *   kubectl -n backend-stage exec -i chain-POD -- node < scripts/full-export.js > ~/Desktop/substrate-full-export.json
 *
 * Or for prod:
 *   Change WS_URL below to wss://mainnet.liquidity.io
 */

const WS_URL = process.env.WS_URL || 'wss://blockchain09.satschel.com';
const WebSocket = require('ws');

let ws;
let rpcId = 0;
const pending = new Map();

function connect() {
  return new Promise((resolve, reject) => {
    ws = new WebSocket(WS_URL);
    ws.on('open', resolve);
    ws.on('error', reject);
    ws.on('message', (data) => {
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

function rpc(method, params = [], timeoutMs = 60000) {
  const id = ++rpcId;
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => {
      pending.delete(id);
      reject(new Error(`timeout: ${method} id=${id}`));
    }, timeoutMs);
    pending.set(id, {
      resolve: (v) => { clearTimeout(timer); resolve(v); },
      reject: (e) => { clearTimeout(timer); reject(e); },
    });
    ws.send(JSON.stringify({ jsonrpc: '2.0', id, method, params }));
  });
}

async function rpcRetry(method, params = [], retries = 2, timeoutMs = 30000) {
  for (let i = 0; i < retries; i++) {
    try {
      return await rpc(method, params, timeoutMs);
    } catch (e) {
      if (i === retries - 1) throw e;
      console.error(`  retry ${i + 1}/${retries} for ${method} (timeout=${timeoutMs}ms)`);
      await new Promise(r => setTimeout(r, 1000));
      timeoutMs = Math.min(timeoutMs * 2, 60000);
    }
  }
}

(async () => {
  console.error(`Connecting to ${WS_URL}...`);
  await connect();

  const chain = await rpc('system_chain');
  const version = await rpc('system_version');
  const runtime = await rpc('state_getRuntimeVersion');
  const header = await rpc('chain_getHeader');
  const blockNum = parseInt(header.number, 16);
  const blockHash = await rpc('chain_getBlockHash', [blockNum]);
  console.error(`Chain: ${chain} v${version} spec=${runtime?.specVersion}`);
  console.error(`Block: #${blockNum} hash=${blockHash}`);
  console.error(`State root: ${header.stateRoot}`);

  // Enumerate ALL storage keys at finalized head
  console.error('\nEnumerating ALL storage keys...');
  const allKeys = await rpc('state_getKeys', ['0x', blockHash]);
  console.error(`Total storage keys: ${allKeys.length}`);

  // Group by pallet prefix (first 16 bytes = 32 hex after 0x)
  const byPallet = {};
  for (const key of allKeys) {
    const prefix = key.slice(0, 34);
    if (!byPallet[prefix]) byPallet[prefix] = [];
    byPallet[prefix].push(key);
  }

  // Group by storage prefix (first 32 bytes = 64 hex after 0x)
  const byStorage = {};
  for (const key of allKeys) {
    const prefix = key.length >= 66 ? key.slice(0, 66) : key;
    if (!byStorage[prefix]) byStorage[prefix] = [];
    byStorage[prefix].push(key);
  }

  console.error(`\nPallet prefixes: ${Object.keys(byPallet).length}`);
  for (const [p, keys] of Object.entries(byPallet).sort((a, b) => b[1].length - a[1].length)) {
    console.error(`  ${p}: ${keys.length} keys`);
  }

  console.error(`\nStorage prefixes: ${Object.keys(byStorage).length}`);

  // Fetch ALL values — one at a time with retry to avoid timeouts
  console.error('\nFetching ALL storage values...');
  const entries = [];
  let fetched = 0;
  let errors = 0;

  for (const key of allKeys) {
    try {
      const value = await rpcRetry('state_getStorage', [key, blockHash]);
      entries.push({ k: key, v: value });
    } catch (e) {
      console.error(`  ERROR key=${key.slice(0, 40)}...: ${e.message}`);
      entries.push({ k: key, v: null, error: e.message });
      errors++;
    }
    fetched++;
    if (fetched % 500 === 0) {
      console.error(`  ${fetched}/${allKeys.length} (${errors} errors)`);
    }
  }
  console.error(`\nDone: ${fetched} fetched, ${errors} errors`);

  // Build per-pallet grouped output
  const pallets = {};
  for (const entry of entries) {
    const prefix = entry.k.slice(0, 34);
    if (!pallets[prefix]) pallets[prefix] = { count: 0, entries: [] };
    pallets[prefix].count++;
    pallets[prefix].entries.push(entry);
  }

  // Metadata hex for offline decoding
  console.error('Fetching runtime metadata...');
  const metadata = await rpcRetry('state_getMetadata', [blockHash]);

  // Output
  const output = {
    _meta: {
      chain, version,
      specName: runtime?.specName,
      specVersion: runtime?.specVersion,
      blockNumber: blockNum,
      blockHash,
      stateRoot: header.stateRoot,
      parentHash: header.parentHash,
      exportedAt: new Date().toISOString(),
      wsEndpoint: WS_URL,
      totalKeys: allKeys.length,
      totalFetched: fetched,
      totalErrors: errors,
    },
    palletBreakdown: Object.fromEntries(
      Object.entries(byPallet).map(([p, keys]) => [p, keys.length])
    ),
    storageBreakdown: Object.fromEntries(
      Object.entries(byStorage).map(([p, keys]) => [p, keys.length])
    ),
    pallets,
    metadata,
  };

  // Write to stdout
  console.log(JSON.stringify(output));

  console.error('\n=== Export Complete ===');
  console.error(`Total keys:    ${allKeys.length}`);
  console.error(`Fetched:       ${fetched}`);
  console.error(`Errors:        ${errors}`);
  console.error(`Pallet groups: ${Object.keys(pallets).length}`);

  ws.close();
})().catch(e => {
  console.error('FATAL:', e);
  process.exit(1);
});
