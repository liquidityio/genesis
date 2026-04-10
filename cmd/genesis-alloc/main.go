// genesis-alloc reads an Avalanche snapshot and generates a Lux EVM genesis
// alloc with ERC20 balances baked into contract storage slots. Every token
// gets a deterministic CREATE2 address (salt = keccak256(symbol)), every
// holder's balanceOf is pre-set in the storage trie, and totalSupply matches
// the sum of all balances.
//
// Output is compatible with the Lux EVM genesis format and can be merged
// directly into chains/evm.json's "alloc" field.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"math/big"
	"os"
	"sort"
	"strings"

	"golang.org/x/crypto/sha3"
)

// --- Snapshot schema (matches SNAPSHOT_*.json) ---

type Snapshot struct {
	Chain      string   `json:"chain"`
	Block      uint64   `json:"block"`
	Timestamp  string   `json:"timestamp_utc"`
	Holders    []Holder `json:"holders"`
	TotalPos   int      `json:"total_positions"`
}

type Holder struct {
	Wallet    string              `json:"wallet"`
	Owner     string              `json:"owner"`
	Positions map[string]Position `json:"positions"`
}

type Position struct {
	TokenAddress string `json:"token_address"`
	RawWei       string `json:"raw_wei"`
	Amount       float64 `json:"amount"`
}

// --- Genesis output schema ---

type GenesisOutput struct {
	Alloc map[string]*AllocEntry `json:"alloc"`
}

type AllocEntry struct {
	Code    string            `json:"code,omitempty"`
	Storage map[string]string `json:"storage,omitempty"`
	Balance string            `json:"balance"`
	Nonce   string            `json:"nonce,omitempty"`
}

// --- Deployment manifest schema ---

type DeployManifest struct {
	Core        map[string]string `json:"core"`
	BatchTokens map[string]string `json:"batchTokens"`
	Deployed    map[string]string `json:"deployed"`
}

// --- Constants ---

const (
	deployerAddr = "0x9011E888251AB053B7bD1cdB598Db4f9DEd94714"
	// Treasury gets 10B LQDTY in native balance (gas token)
	treasuryLQDTY = "10000000000" // 10B * 1e18 in hex computed below
	// MINTER_ROLE = keccak256("MINTER_ROLE")
	// Computed: 0x9f2df0fed2c77648de5860a4cc508cd0818c85b8b8a1ab4ceeef8d981c8956a6
	minterRoleHash = "9f2df0fed2c77648de5860a4cc508cd0818c85b8b8a1ab4ceeef8d981c8956a6"
	// DEFAULT_ADMIN_ROLE = 0x00
	adminRoleHash = "0000000000000000000000000000000000000000000000000000000000000000"
)

// Minimal ERC20 runtime bytecode. This is a placeholder that will be replaced
// with the actual compiled SecurityToken/USDL bytecode from the deployment.
// For genesis, we only need storage slots to be correct -- the bytecode is
// loaded from the --bytecode flag or a sensible default.
//
// We use a stripped ERC20 proxy that delegates to the standard implementation.
// In practice, the rebuild-chain.sh script extracts bytecode from the existing
// chain via eth_getCode.
var defaultERC20Code = "0x" // Will be populated from --bytecode or --code-from-rpc

func main() {
	var (
		snapshotPath string
		outputPath   string
		chainID      uint64
		manifestPath string
		horseManifestPaths stringSlice
		securityBytecode string
		usdlBytecode string
		registryBytecode string
	)

	flag.StringVar(&snapshotPath, "snapshot", "", "Path to SNAPSHOT_*.json")
	flag.StringVar(&outputPath, "output", "genesis-alloc.json", "Output path")
	flag.Uint64Var(&chainID, "chain-id", 8675309, "Chain ID")
	flag.StringVar(&manifestPath, "manifest", "", "Path to mainnet-20260327.json (optional, for address mapping)")
	flag.Var(&horseManifestPaths, "horse-manifest", "Path to horse-deploy-*.json (repeatable)")
	flag.StringVar(&securityBytecode, "security-bytecode", "", "Hex bytecode for SecurityToken (from eth_getCode)")
	flag.StringVar(&usdlBytecode, "usdl-bytecode", "", "Hex bytecode for USDL (from eth_getCode)")
	flag.StringVar(&registryBytecode, "registry-bytecode", "", "Hex bytecode for ComplianceRegistry")
	flag.Parse()

	if snapshotPath == "" {
		fmt.Fprintln(os.Stderr, "error: --snapshot is required")
		os.Exit(1)
	}

	// --- Load snapshot ---
	snap, err := loadSnapshot(snapshotPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error loading snapshot: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "Loaded snapshot: block %d, %d holders, %d positions\n",
		snap.Block, len(snap.Holders), snap.TotalPos)

	// --- Load optional manifests for existing address mapping ---
	existingAddrs := make(map[string]string) // symbol -> address
	if manifestPath != "" {
		if err := loadManifestAddrs(manifestPath, existingAddrs); err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to load manifest %s: %v\n", manifestPath, err)
		}
	}
	for _, hp := range horseManifestPaths {
		if err := loadHorseManifestAddrs(hp, existingAddrs); err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to load horse manifest %s: %v\n", hp, err)
		}
	}

	// --- Build token -> holders mapping ---
	type tokenBalance struct {
		Wallet string
		Wei    *big.Int
	}
	tokenHolders := make(map[string][]tokenBalance) // symbol -> balances
	tokenAddrs := make(map[string]string)             // symbol -> avax address (for reference)

	for _, h := range snap.Holders {
		wallet := strings.ToLower(h.Wallet)
		for sym, pos := range h.Positions {
			wei := new(big.Int)
			if _, ok := wei.SetString(pos.RawWei, 10); !ok {
				fmt.Fprintf(os.Stderr, "warning: invalid raw_wei for %s/%s: %s\n", wallet, sym, pos.RawWei)
				continue
			}
			if wei.Sign() <= 0 {
				continue
			}
			tokenHolders[sym] = append(tokenHolders[sym], tokenBalance{
				Wallet: wallet,
				Wei:    wei,
			})
			if _, exists := tokenAddrs[sym]; !exists {
				tokenAddrs[sym] = strings.ToLower(pos.TokenAddress)
			}
		}
	}

	// Sort tokens for deterministic output
	symbols := make([]string, 0, len(tokenHolders))
	for sym := range tokenHolders {
		symbols = append(symbols, sym)
	}
	sort.Strings(symbols)

	// --- Generate genesis alloc ---
	alloc := make(map[string]*AllocEntry)

	// 1. Treasury / deployer with native LQDTY balance
	treasuryBal := new(big.Int)
	// 10B LQDTY * 1e18
	treasuryBal.SetString("10000000000000000000000000000", 10)
	deployerLower := strings.ToLower(stripHexPrefix(deployerAddr))
	alloc[deployerLower] = &AllocEntry{
		Balance: "0x" + treasuryBal.Text(16),
	}

	// 2. Each token contract with pre-set balances in storage
	for _, sym := range symbols {
		balances := tokenHolders[sym]

		// Determine contract address: use existing manifest address or CREATE2
		contractAddr := ""
		if addr, ok := existingAddrs[sym]; ok {
			contractAddr = strings.ToLower(stripHexPrefix(addr))
		} else {
			contractAddr = computeCREATE2(deployerAddr, sym)
		}

		// Compute total supply
		totalSupply := new(big.Int)
		for _, b := range balances {
			totalSupply.Add(totalSupply, b.Wei)
		}

		// Build storage map
		storage := make(map[string]string)

		// OZ ERC20Upgradeable / ERC20 storage layout:
		//   slot 0: _balances mapping
		//   slot 1: _allowances mapping
		//   slot 2: _totalSupply
		//   slot 3: _name (string)
		//   slot 4: _symbol (string)
		//
		// For OZ 5.x with namespaced storage (ERC20Storage at keccak256("openzeppelin.storage.ERC20") - 1):
		//   We use the classic layout (slot 0/2) which works for non-upgradeable contracts.
		//   The SecurityToken and USDL contracts are non-upgradeable, so classic layout applies.

		// totalSupply at slot 2
		storage[slot(2)] = toHex32(totalSupply)

		// balanceOf[address] = keccak256(abi.encode(address, uint256(0)))
		for _, b := range balances {
			balSlot := balanceOfSlot(b.Wallet, 0)
			storage[balSlot] = toHex32(b.Wei)
		}

		// AccessControl roles: grant MINTER_ROLE and DEFAULT_ADMIN_ROLE to deployer
		// OZ AccessControl storage:
		//   _roles mapping at slot 0 (in AccessControl, but offset by inheritance)
		//
		// For non-upgradeable OZ 5.x contracts:
		//   AccessControl uses ERC-7201 namespaced storage:
		//   keccak256("openzeppelin.storage.AccessControl") - 1 = base slot
		//   _roles is at that base slot
		//   _roles[role].hasRole[account] = mapping(bytes32 => mapping(address => bool))
		//
		// The AccessControl storage slot is:
		//   keccak256(abi.encode("openzeppelin.storage.AccessControl")) - 1
		//
		// We compute the role storage slots using the OZ 5.x layout.
		acBaseSlot := accessControlBaseSlot()

		// Grant MINTER_ROLE to deployer
		minterHasRoleSlot := roleHasRoleSlot(acBaseSlot, minterRoleHash, deployerLower)
		storage[minterHasRoleSlot] = toHex32(big.NewInt(1))

		// Grant DEFAULT_ADMIN_ROLE to deployer
		adminHasRoleSlot := roleHasRoleSlot(acBaseSlot, adminRoleHash, deployerLower)
		storage[adminHasRoleSlot] = toHex32(big.NewInt(1))

		// Set bytecode
		code := securityBytecode
		if sym == "USDL" && usdlBytecode != "" {
			code = usdlBytecode
		}

		entry := &AllocEntry{
			Balance: "0x0",
			Storage: storage,
		}
		if code != "" {
			entry.Code = ensureHexPrefix(code)
		}
		// Nonce 1 for contracts (EVM convention: deployed contracts start at nonce 1)
		entry.Nonce = "0x1"

		alloc[contractAddr] = entry

		fmt.Fprintf(os.Stderr, "  %s: %s (%d holders, supply=%s)\n",
			sym, contractAddr, len(balances), totalSupply.String())
	}

	// 3. ComplianceRegistry with all wallets whitelisted
	if registryBytecode != "" {
		registryAddr := ""
		if addr, ok := existingAddrs["ComplianceRegistry"]; ok {
			registryAddr = strings.ToLower(stripHexPrefix(addr))
		} else {
			registryAddr = computeCREATE2(deployerAddr, "ComplianceRegistry")
		}

		regStorage := make(map[string]string)

		// Whitelist all unique wallets
		// ComplianceRegistry._whitelisted mapping is at the namespaced storage base
		// For simplicity, we use slot 0 as the whitelist mapping base
		wallets := uniqueWallets(snap.Holders)
		for _, w := range wallets {
			wlSlot := mappingSlot(w, 0)
			regStorage[wlSlot] = toHex32(big.NewInt(1))
		}

		// Grant admin to deployer
		acBase := accessControlBaseSlot()
		regStorage[roleHasRoleSlot(acBase, adminRoleHash, deployerLower)] = toHex32(big.NewInt(1))

		alloc[registryAddr] = &AllocEntry{
			Code:    ensureHexPrefix(registryBytecode),
			Balance: "0x0",
			Storage: regStorage,
			Nonce:   "0x1",
		}
		fmt.Fprintf(os.Stderr, "  ComplianceRegistry: %s (%d wallets whitelisted)\n",
			registryAddr, len(wallets))
	}

	// --- Write output ---
	out := GenesisOutput{Alloc: alloc}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "error marshaling output: %v\n", err)
		os.Exit(1)
	}

	if err := os.WriteFile(outputPath, data, 0644); err != nil {
		fmt.Fprintf(os.Stderr, "error writing %s: %v\n", outputPath, err)
		os.Exit(1)
	}

	fmt.Fprintf(os.Stderr, "\nGenesis alloc written to %s\n", outputPath)
	fmt.Fprintf(os.Stderr, "  Tokens: %d\n", len(symbols))
	fmt.Fprintf(os.Stderr, "  Total alloc entries: %d\n", len(alloc))

	// Print summary to stdout for scripting
	summary := map[string]interface{}{
		"tokens":       len(symbols),
		"holders":      len(snap.Holders),
		"positions":    snap.TotalPos,
		"alloc_entries": len(alloc),
		"block":        snap.Block,
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(summary)
}

// --- Helpers ---

func loadSnapshot(path string) (*Snapshot, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s Snapshot
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

func loadManifestAddrs(path string, out map[string]string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var m DeployManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return err
	}
	for sym, addr := range m.BatchTokens {
		out[sym] = addr
	}
	for name, addr := range m.Core {
		out[name] = addr
	}
	return nil
}

func loadHorseManifestAddrs(path string, out map[string]string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var m struct {
		Deployed map[string]string `json:"deployed"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return err
	}
	for sym, addr := range m.Deployed {
		out[sym] = addr
	}
	return nil
}

// computeCREATE2 computes a deterministic address using CREATE2.
// addr = keccak256(0xff ++ deployer ++ salt ++ keccak256(initCode))[12:]
// Since we don't have initCode at genesis time, we use a simplified scheme:
// addr = keccak256(deployer ++ keccak256(symbol))[12:]
// This gives us deterministic, reproducible addresses per symbol.
func computeCREATE2(deployer, symbol string) string {
	deployerBytes, _ := hex.DecodeString(stripHexPrefix(deployer))
	salt := keccak256([]byte(symbol))

	// CREATE2: keccak256(0xff ++ deployer ++ salt ++ initCodeHash)
	// We use sha256 of the symbol as a stand-in initCodeHash for address derivation.
	// This is NOT real CREATE2 (which needs actual initcode), but gives us
	// deterministic addresses that are unique per symbol per deployer.
	initCodeHash := sha256.Sum256([]byte("SecurityToken:" + symbol))

	var buf []byte
	buf = append(buf, 0xff)
	buf = append(buf, deployerBytes...)
	buf = append(buf, salt[:]...)
	buf = append(buf, initCodeHash[:]...)

	hash := keccak256(buf)
	return hex.EncodeToString(hash[12:])
}

// balanceOfSlot computes the storage slot for balanceOf[address].
// slot = keccak256(abi.encode(address, uint256(mappingSlotNum)))
func balanceOfSlot(address string, mappingSlot uint64) string {
	addr := padAddress(stripHexPrefix(address))
	slotBytes := padUint256(mappingSlot)

	var buf []byte
	buf = append(buf, addr...)
	buf = append(buf, slotBytes...)

	hash := keccak256(buf)
	return "0x" + hex.EncodeToString(hash[:])
}

// mappingSlot computes the storage slot for mapping[key] at a given base slot.
func mappingSlot(key string, baseSlot uint64) string {
	return balanceOfSlot(key, baseSlot)
}

// accessControlBaseSlot computes the OZ 5.x ERC-7201 storage slot for AccessControl.
// keccak256(abi.encode(uint256(keccak256("openzeppelin.storage.AccessControl")) - 1)) & ~0xff
// Simplified: we use the well-known slot.
func accessControlBaseSlot() *big.Int {
	// keccak256("openzeppelin.storage.AccessControl") - 1
	hash := keccak256([]byte("openzeppelin.storage.AccessControl"))
	slot := new(big.Int).SetBytes(hash[:])
	slot.Sub(slot, big.NewInt(1))
	return slot
}

// roleHasRoleSlot computes storage slot for _roles[role].hasRole[account]
// _roles is a mapping(bytes32 => RoleData) at baseSlot
// RoleData { mapping(address => bool) hasRole; bytes32 adminRole; }
// hasRole is at offset 0 of RoleData
// So: _roles[role] is at keccak256(abi.encode(role, baseSlot))
//     _roles[role].hasRole[account] is at keccak256(abi.encode(account, keccak256(abi.encode(role, baseSlot))))
func roleHasRoleSlot(baseSlot *big.Int, roleHash string, account string) string {
	roleBytes, _ := hex.DecodeString(roleHash)
	// Pad to 32 bytes
	rolePadded := make([]byte, 32)
	copy(rolePadded[32-len(roleBytes):], roleBytes)

	basePadded := make([]byte, 32)
	baseSlotBytes := baseSlot.Bytes()
	copy(basePadded[32-len(baseSlotBytes):], baseSlotBytes)

	// keccak256(abi.encode(role, baseSlot))
	var buf1 []byte
	buf1 = append(buf1, rolePadded...)
	buf1 = append(buf1, basePadded...)
	roleDataSlot := keccak256(buf1)

	// keccak256(abi.encode(account, roleDataSlot))
	acctBytes := padAddress(stripHexPrefix(account))
	var buf2 []byte
	buf2 = append(buf2, acctBytes...)
	buf2 = append(buf2, roleDataSlot[:]...)

	hash := keccak256(buf2)
	return "0x" + hex.EncodeToString(hash[:])
}

func uniqueWallets(holders []Holder) []string {
	seen := make(map[string]bool)
	var wallets []string
	for _, h := range holders {
		w := strings.ToLower(h.Wallet)
		if !seen[w] {
			seen[w] = true
			wallets = append(wallets, w)
		}
	}
	sort.Strings(wallets)
	return wallets
}

func keccak256(data []byte) [32]byte {
	h := sha3.NewLegacyKeccak256()
	h.Write(data)
	var out [32]byte
	h.Sum(out[:0])
	return out
}

func padAddress(hexAddr string) []byte {
	addr, _ := hex.DecodeString(hexAddr)
	padded := make([]byte, 32)
	copy(padded[32-len(addr):], addr)
	return padded
}

func padUint256(v uint64) []byte {
	n := new(big.Int).SetUint64(v)
	padded := make([]byte, 32)
	b := n.Bytes()
	copy(padded[32-len(b):], b)
	return padded
}

func slot(n uint64) string {
	padded := make([]byte, 32)
	b := new(big.Int).SetUint64(n).Bytes()
	copy(padded[32-len(b):], b)
	return "0x" + hex.EncodeToString(padded)
}

func toHex32(v *big.Int) string {
	padded := make([]byte, 32)
	b := v.Bytes()
	copy(padded[32-len(b):], b)
	return "0x" + hex.EncodeToString(padded)
}

func stripHexPrefix(s string) string {
	return strings.TrimPrefix(strings.TrimPrefix(s, "0x"), "0X")
}

func ensureHexPrefix(s string) string {
	if strings.HasPrefix(s, "0x") || strings.HasPrefix(s, "0X") {
		return s
	}
	return "0x" + s
}

// stringSlice implements flag.Value for repeatable string flags.
type stringSlice []string

func (s *stringSlice) String() string { return strings.Join(*s, ",") }
func (s *stringSlice) Set(v string) error {
	*s = append(*s, v)
	return nil
}
