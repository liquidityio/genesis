#!/usr/bin/env -S npx tsx
/**
 * verify-export.ts
 *
 * Verifies a substrate export JSON against the live chain.
 * Spot-checks N random accounts to confirm balances match.
 *
 * Usage:
 *   npx tsx scripts/verify-export.ts substrate-export-12345.json [--rpc wss://mainnet.liquidity.io] [--samples 50]
 */

import * as fs from 'fs';
import { WebSocket } from 'ws';

const EXPORT_FILE = process.argv[2];
if (!EXPORT_FILE) {
  console.error('Usage: verify-export.ts <export-file.json> [--rpc URL] [--samples N]');
  process.exit(1);
}

const RPC_URL = process.argv.find(a => a.startsWith('--rpc='))?.split('=')[1] ?? 'ws://127.0.0.1:9944';
const SAMPLES = parseInt(process.argv.find(a => a.startsWith('--samples='))?.split('=')[1] ?? '50');

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

function leHexToDecimal(hex: string): string {
  if (!hex || hex === '0x' || hex === '') return '0';
  const clean = hex.replace('0x', '');
  const bytes = clean.match(/.{2}/g)?.reverse().join('') ?? '0';
  return BigInt('0x' + bytes).toString();
}

async function main() {
  const data = JSON.parse(fs.readFileSync(EXPORT_FILE, 'utf-8'));
  const meta = data._meta;
  console.log(`\n=== Verifying Export ===`);
  console.log(`Export block: #${meta.blockNumber} (${meta.blockHash})`);
  console.log(`Accounts in export: ${data.accounts.count}`);
  console.log(`Sampling ${SAMPLES} accounts against ${RPC_URL}\n`);

  await connectWs(RPC_URL);

  const accounts = data.accounts.entries;
  const sampleIndices = new Set<number>();
  while (sampleIndices.size < Math.min(SAMPLES, accounts.length)) {
    sampleIndices.add(Math.floor(Math.random() * accounts.length));
  }

  let pass = 0;
  let fail = 0;

  for (const idx of sampleIndices) {
    const acct = accounts[idx];
    const liveValue = await rpc('state_getStorage', [acct.storageKey, meta.blockHash]);

    if (!liveValue) {
      if (acct.free === '0' && acct.reserved === '0') {
        pass++;
      } else {
        console.log(`  FAIL: ${acct.pubKeyHex} — export has balance but chain returns null`);
        fail++;
      }
      continue;
    }

    const dataHex = liveValue.replace('0x', '');
    const liveFree = leHexToDecimal(dataHex.slice(32, 64));
    const liveReserved = leHexToDecimal(dataHex.slice(64, 96));

    if (liveFree === acct.free && liveReserved === acct.reserved) {
      pass++;
    } else {
      console.log(`  FAIL: ${acct.pubKeyHex}`);
      console.log(`    Export:  free=${acct.free} reserved=${acct.reserved}`);
      console.log(`    Chain:   free=${liveFree} reserved=${liveReserved}`);
      fail++;
    }
  }

  console.log(`\n=== Results ===`);
  console.log(`Pass: ${pass}/${pass + fail}`);
  console.log(`Fail: ${fail}/${pass + fail}`);
  console.log(`State root in export: ${meta.stateRoot}`);

  // Verify state root matches
  const header = await rpc('chain_getHeader', [meta.blockHash]);
  if (header.stateRoot === meta.stateRoot) {
    console.log(`State root matches chain: OK`);
  } else {
    console.log(`State root MISMATCH! Export=${meta.stateRoot} Chain=${header.stateRoot}`);
  }

  ws.close();
  process.exit(fail > 0 ? 1 : 0);
}

main().catch(err => {
  console.error('Verify failed:', err);
  process.exit(1);
});
