#!/usr/bin/env python3
"""
export.py — Export all LQDTY Substrate mainnet state.

Connects to the Substrate node via GKE pod kubectl exec and dumps
all account balances and loan records to JSON.

Usage:
  python3 scripts/export.py [--pod PODNAME] [--namespace backend]

Output: lqdty-mainnet-export.json

Prerequisites:
  - gcloud auth + kubectl access to apps-gke cluster (us-central1)
  - Your IP in GKE master authorized networks
"""

import json
import os
import subprocess
import sys
import argparse

SCRIPT_DIR = os.path.dirname(os.path.abspath(__file__))
REPO_DIR = os.path.dirname(SCRIPT_DIR)
OUTPUT = os.path.join(REPO_DIR, "lqdty-mainnet-export.json")

# Node.js script that runs inside the GKE pod (the pod has ws + node)
EXPORT_JS = r"""
const WebSocket = require('ws');
const ws = new WebSocket('wss://mainnet.liquidity.io');
let id = 0;
function rpc(method, params=[]) {
  return new Promise((res,rej) => {
    const myId = ++id;
    const handler = (data) => {
      const msg = JSON.parse(data.toString());
      if (msg.id === myId) { ws.removeListener('message', handler); res(msg.result); }
    };
    ws.on('message', handler);
    ws.send(JSON.stringify({jsonrpc:'2.0',id:myId,method,params}));
    setTimeout(() => rej('timeout'), 60000);
  });
}

ws.on('open', async () => {
  try {
    const header = await rpc('chain_getHeader');
    const blockNum = parseInt(header.number, 16);
    console.error('Block: ' + blockNum);

    console.error('Exporting accounts...');
    const accKeys = await rpc('state_getKeys', [
      '0x26aa394eea5630e07c48ae0c9558cef7b99d880ec681799c0cf30e8886371da9'
    ]);
    const accounts = [];
    for (let i = 0; i < accKeys.length; i++) {
      const data = await rpc('state_getStorage', [accKeys[i]]);
      if (!data) continue;
      const pubKey = accKeys[i].replace('0x','').slice(-64);
      const d = data.replace('0x','');
      if (d.length >= 96) {
        const free = BigInt('0x' + d.slice(32,64).match(/.{2}/g).reverse().join(''));
        const reserved = BigInt('0x' + d.slice(64,96).match(/.{2}/g).reverse().join(''));
        if (free > 0n || reserved > 0n) {
          accounts.push({p: pubKey, f: free.toString(), r: reserved.toString()});
        }
      }
      if (i % 200 === 0) console.error('  accounts: ' + i + '/' + accKeys.length);
    }
    console.error('Accounts: ' + accounts.length);

    console.error('Scanning for loans...');
    const allKeys = await rpc('state_getKeys', ['0x']);
    const sysPrefix = '26aa394eea5630e07c48ae0c9558cef7';
    const nonSys = allKeys.filter(k => !k.replace('0x','').startsWith(sysPrefix));
    console.error('Non-system keys: ' + nonSys.length);

    const loans = [];
    for (let batch = 0; batch < nonSys.length; batch += 10) {
      const promises = nonSys.slice(batch, batch+10).map(async (key) => {
        const val = await rpc('state_getStorage', [key]);
        if (!val || val.length < 100) return null;
        const buf = Buffer.from(val.replace('0x',''), 'hex');
        let asc = '';
        for (let j = 0; j < Math.min(buf.length, 1500); j++) {
          asc += (buf[j] >= 32 && buf[j] < 127) ? String.fromCharCode(buf[j]) : ' ';
        }
        asc = asc.replace(/\s+/g, ' ').trim();
        if (/AUTO|HYUNDAI|TOYOTA|CHEVROLET|FORD|HONDA|BMW|NISSAN|Complete|Approved|Funded|ContractSigned|Declined|Servicing|PaidOff|Anchored/.test(asc)) {
          return {key, ascii: asc.substring(0, 600), len: val.length};
        }
        return null;
      });
      const results = await Promise.all(promises);
      results.filter(Boolean).forEach(r => loans.push(r));
      if (batch % 500 === 0) console.error('  scan: ' + batch + '/' + nonSys.length + ' loans:' + loans.length);
    }
    console.error('Loans: ' + loans.length);

    console.log(JSON.stringify({
      chain:'LQDTY', block: blockNum, timestamp: new Date().toISOString(),
      accounts, loans,
    }));
    ws.close();
  } catch(e) { console.error('ERR: ' + e); ws.close(); process.exit(1); }
});
ws.on('error', (e) => { console.error('WS Error:', e.message); process.exit(1); });
"""


def ensure_gke_credentials():
    """Ensure kubectl can reach the GKE cluster."""
    print("Fetching GKE credentials...")
    subprocess.run(
        ["gcloud", "container", "clusters", "get-credentials",
         "apps-gke", "--region", "us-central1"],
        capture_output=True, check=True,
    )


def find_chain_pod(namespace: str) -> str:
    """Find a running chain pod in the namespace."""
    result = subprocess.run(
        ["kubectl", "-n", namespace, "get", "pods",
         "-l", "app=chain", "-o", "jsonpath={.items[0].metadata.name}"],
        capture_output=True, text=True, timeout=15,
    )
    if result.returncode == 0 and result.stdout.strip():
        return result.stdout.strip()

    # Fallback: search by name
    result = subprocess.run(
        ["kubectl", "-n", namespace, "get", "pods", "--no-headers"],
        capture_output=True, text=True, timeout=15,
    )
    for line in result.stdout.strip().split("\n"):
        name = line.split()[0]
        if name.startswith("chain-") and "Running" in line:
            return name

    raise RuntimeError(f"No running chain pod found in namespace {namespace}")


def export_via_pod(namespace: str, pod: str):
    """Run the export JS inside the pod and capture output."""
    print(f"Exporting via {namespace}/{pod}...")
    print("This takes 5-10 minutes (scanning ~7000 storage keys)...\n")

    result = subprocess.run(
        ["kubectl", "-n", namespace, "exec", pod, "-c", "chain",
         "--", "node", "-e", EXPORT_JS],
        capture_output=True, text=True, timeout=900,  # 15 min max
    )

    # Print progress (stderr)
    for line in result.stderr.strip().split("\n"):
        if line:
            print(f"  {line}")

    if result.returncode != 0:
        print(f"\nExport failed (exit {result.returncode})")
        sys.exit(1)

    return result.stdout


def main():
    parser = argparse.ArgumentParser(description="Export LQDTY Substrate state")
    parser.add_argument("--namespace", default="backend", help="K8s namespace")
    parser.add_argument("--pod", default="", help="Pod name (auto-detected if empty)")
    args = parser.parse_args()

    ensure_gke_credentials()

    pod = args.pod or find_chain_pod(args.namespace)
    print(f"Using pod: {args.namespace}/{pod}\n")

    raw = export_via_pod(args.namespace, pod)

    # Parse and validate
    data = json.loads(raw)
    accounts = len(data.get("accounts", []))
    loans = len(data.get("loans", []))

    # Write output
    with open(OUTPUT, "w") as f:
        json.dump(data, f, indent=2)

    size = os.path.getsize(OUTPUT)
    print(f"\n=== Export Complete ===")
    print(f"Block:    {data.get('block', '?')}")
    print(f"Accounts: {accounts}")
    print(f"Loans:    {loans}")
    print(f"Output:   {OUTPUT} ({size:,} bytes)")
    print(f"\nNext: python3 scripts/redenominate.py")


if __name__ == "__main__":
    main()
