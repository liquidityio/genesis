// hourly-mint diffs two Avalanche snapshots and mints ONLY new/increased
// positions on the Liquidity EVM. Monotonically increasing: decreased
// positions are ignored (the user sold on Avalanche, their EVM balance stays).
//
// Idempotent: checks on-chain balanceOf before minting. Safe to re-run.
//
// Usage:
//
//	go run ./cmd/hourly-mint/ \
//	  --previous SNAPSHOT_82552152.json \
//	  --current  SNAPSHOT_LATEST.json \
//	  --rpc http://localhost:9631/ext/bc/.../rpc \
//	  --private-key $KEY \
//	  --manifest horse-deploy-mainnet-*.json
package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"golang.org/x/crypto/sha3"
)

// --- Snapshot schema ---

type Snapshot struct {
	Chain     string   `json:"chain"`
	Block     uint64   `json:"block"`
	Timestamp string   `json:"timestamp_utc"`
	Holders   []Holder `json:"holders"`
}

type Holder struct {
	Wallet    string              `json:"wallet"`
	Owner     string              `json:"owner"`
	Positions map[string]Position `json:"positions"`
}

type Position struct {
	TokenAddress string  `json:"token_address"`
	RawWei       string  `json:"raw_wei"`
	Amount       float64 `json:"amount"`
}

// MintAction represents a single mint to execute.
type MintAction struct {
	Symbol        string   `json:"symbol"`
	TokenContract string   `json:"token_contract"`
	Recipient     string   `json:"recipient"`
	Amount        *big.Int `json:"amount"`
	Reason        string   `json:"reason"` // "new_position" or "increased_position"
}

// MintResult records the outcome of a mint.
type MintResult struct {
	MintAction
	TxHash    string `json:"tx_hash,omitempty"`
	Error     string `json:"error,omitempty"`
	Skipped   bool   `json:"skipped,omitempty"`
	Timestamp string `json:"timestamp"`
}

func main() {
	var (
		previousPath       string
		currentPath        string
		rpcURL             string
		privateKey         string
		manifestPath       string
		horseManifestPaths stringSlice
		dryRun             bool
		outputPath         string
	)

	flag.StringVar(&previousPath, "previous", "", "Previous snapshot JSON")
	flag.StringVar(&currentPath, "current", "", "Current snapshot JSON")
	flag.StringVar(&rpcURL, "rpc", "", "Liquidity EVM RPC URL")
	flag.StringVar(&privateKey, "private-key", "", "Deployer private key (hex, no 0x prefix)")
	flag.StringVar(&manifestPath, "manifest", "", "Deployment manifest for token addresses")
	flag.Var(&horseManifestPaths, "horse-manifest", "Horse deploy manifest (repeatable)")
	flag.BoolVar(&dryRun, "dry-run", false, "Compute deltas without sending transactions")
	flag.StringVar(&outputPath, "output", "", "Write mint results to JSON file")
	flag.Parse()

	if previousPath == "" || currentPath == "" {
		fmt.Fprintln(os.Stderr, "error: --previous and --current are required")
		os.Exit(1)
	}
	if !dryRun && (rpcURL == "" || privateKey == "") {
		fmt.Fprintln(os.Stderr, "error: --rpc and --private-key required (or use --dry-run)")
		os.Exit(1)
	}

	prev, err := loadSnapshot(previousPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error loading previous snapshot: %v\n", err)
		os.Exit(1)
	}
	curr, err := loadSnapshot(currentPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error loading current snapshot: %v\n", err)
		os.Exit(1)
	}

	fmt.Fprintf(os.Stderr, "Previous: block %d (%s)\n", prev.Block, prev.Timestamp)
	fmt.Fprintf(os.Stderr, "Current:  block %d (%s)\n", curr.Block, curr.Timestamp)

	if curr.Block <= prev.Block {
		fmt.Fprintln(os.Stderr, "Current snapshot is not newer than previous. Nothing to do.")
		os.Exit(0)
	}

	// Load address mapping
	tokenAddrs := make(map[string]string)
	if manifestPath != "" {
		loadManifestAddrs(manifestPath, tokenAddrs)
	}
	for _, hp := range horseManifestPaths {
		loadHorseManifestAddrs(hp, tokenAddrs)
	}

	// Build balance maps: symbol -> wallet -> wei
	prevBals := buildBalanceMap(prev)
	currBals := buildBalanceMap(curr)

	// Compute deltas: only mint increases
	var mints []MintAction
	for sym, currWallets := range currBals {
		prevWallets := prevBals[sym]
		if prevWallets == nil {
			prevWallets = make(map[string]*big.Int)
		}

		contractAddr := tokenAddrs[sym]
		if contractAddr == "" {
			contractAddr = findTokenAddr(curr, sym)
			if contractAddr == "" {
				fmt.Fprintf(os.Stderr, "warning: no contract address for %s, skipping\n", sym)
				continue
			}
		}

		for wallet, currBal := range currWallets {
			prevBal := prevWallets[wallet]
			if prevBal == nil {
				prevBal = big.NewInt(0)
			}

			if currBal.Cmp(prevBal) <= 0 {
				continue
			}

			delta := new(big.Int).Sub(currBal, prevBal)
			reason := "increased_position"
			if prevBal.Sign() == 0 {
				reason = "new_position"
			}

			mints = append(mints, MintAction{
				Symbol:        sym,
				TokenContract: contractAddr,
				Recipient:     wallet,
				Amount:        delta,
				Reason:        reason,
			})
		}
	}

	sort.Slice(mints, func(i, j int) bool {
		if mints[i].Symbol != mints[j].Symbol {
			return mints[i].Symbol < mints[j].Symbol
		}
		return mints[i].Recipient < mints[j].Recipient
	})

	fmt.Fprintf(os.Stderr, "\nDelta: %d mints required\n", len(mints))
	if len(mints) == 0 {
		fmt.Fprintln(os.Stderr, "No new positions to mint.")
		os.Exit(0)
	}

	// Summary per token
	symbolCounts := make(map[string]int)
	symbolAmounts := make(map[string]*big.Int)
	for _, m := range mints {
		symbolCounts[m.Symbol]++
		if symbolAmounts[m.Symbol] == nil {
			symbolAmounts[m.Symbol] = new(big.Int)
		}
		symbolAmounts[m.Symbol].Add(symbolAmounts[m.Symbol], m.Amount)
	}
	for _, sym := range sortedKeys(symbolCounts) {
		fmt.Fprintf(os.Stderr, "  %s: %d mints, total %s wei\n",
			sym, symbolCounts[sym], symbolAmounts[sym].String())
	}

	if dryRun {
		fmt.Fprintln(os.Stderr, "\n--dry-run: no transactions sent")
		writeMintResults(outputPath, mints, nil)
		return
	}

	results := executeMints(context.Background(), rpcURL, privateKey, mints)

	var succeeded, failed, skipped int
	for _, r := range results {
		switch {
		case r.Skipped:
			skipped++
		case r.Error != "":
			failed++
			fmt.Fprintf(os.Stderr, "FAILED: %s %s -> %s: %s\n",
				r.Symbol, r.Recipient, r.Amount.String(), r.Error)
		default:
			succeeded++
		}
	}
	fmt.Fprintf(os.Stderr, "\nResults: %d succeeded, %d skipped, %d failed\n",
		succeeded, skipped, failed)

	writeMintResults(outputPath, nil, results)

	if failed > 0 {
		os.Exit(1)
	}
}

// executeMints checks balanceOf then mints via cast send.
func executeMints(_ context.Context, rpcURL, privateKey string, mints []MintAction) []MintResult {
	var results []MintResult

	for i, m := range mints {
		fmt.Fprintf(os.Stderr, "[%d/%d] %s: mint %s to %s ... ",
			i+1, len(mints), m.Symbol, m.Amount.String(), m.Recipient)

		onChainBal, err := castCall(rpcURL, m.TokenContract, m.Recipient)
		if err != nil {
			results = append(results, MintResult{
				MintAction: m,
				Error:      fmt.Sprintf("balanceOf failed: %v", err),
				Timestamp:  time.Now().UTC().Format(time.RFC3339),
			})
			fmt.Fprintln(os.Stderr, "ERROR (balanceOf)")
			continue
		}

		if onChainBal.Cmp(big.NewInt(0)) > 0 {
			results = append(results, MintResult{
				MintAction: m,
				Skipped:    true,
				Timestamp:  time.Now().UTC().Format(time.RFC3339),
			})
			fmt.Fprintln(os.Stderr, "SKIPPED (has balance)")
			continue
		}

		txHash, err := castSendMint(rpcURL, privateKey, m.TokenContract, m.Recipient, m.Amount)
		if err != nil {
			results = append(results, MintResult{
				MintAction: m,
				Error:      err.Error(),
				Timestamp:  time.Now().UTC().Format(time.RFC3339),
			})
			fmt.Fprintln(os.Stderr, "ERROR")
			continue
		}

		results = append(results, MintResult{
			MintAction: m,
			TxHash:     txHash,
			Timestamp:  time.Now().UTC().Format(time.RFC3339),
		})
		fmt.Fprintf(os.Stderr, "OK tx=%s\n", txHash)
	}

	return results
}

// castCall uses `cast call` to read balanceOf.
func castCall(rpcURL, contract, account string) (*big.Int, error) {
	cmd := exec.Command("cast", "call",
		"--rpc-url", rpcURL,
		ensureHexPrefix(contract),
		"balanceOf(address)(uint256)",
		ensureHexPrefix(account),
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("cast call: %w: %s", err, strings.TrimSpace(string(out)))
	}

	result := strings.TrimSpace(string(out))
	result = strings.TrimPrefix(result, "0x")

	// cast returns decimal by default for uint256
	val := new(big.Int)
	if _, ok := val.SetString(result, 0); !ok {
		return big.NewInt(0), nil
	}
	return val, nil
}

// castSendMint uses `cast send` to call mint(address,uint256).
func castSendMint(rpcURL, privateKey, contract, to string, amount *big.Int) (string, error) {
	cmd := exec.Command("cast", "send",
		"--rpc-url", rpcURL,
		"--private-key", privateKey,
		ensureHexPrefix(contract),
		"mint(address,uint256)",
		ensureHexPrefix(to),
		amount.String(),
		"--json",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("cast send: %w: %s", err, strings.TrimSpace(string(out)))
	}

	var resp struct {
		TransactionHash string `json:"transactionHash"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return "", fmt.Errorf("parse cast output: %w: %s", err, string(out))
	}
	return resp.TransactionHash, nil
}

// --- Snapshot helpers ---

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

func buildBalanceMap(snap *Snapshot) map[string]map[string]*big.Int {
	m := make(map[string]map[string]*big.Int)
	for _, h := range snap.Holders {
		wallet := strings.ToLower(h.Wallet)
		for sym, pos := range h.Positions {
			wei := new(big.Int)
			if _, ok := wei.SetString(pos.RawWei, 10); !ok {
				continue
			}
			if wei.Sign() <= 0 {
				continue
			}
			if m[sym] == nil {
				m[sym] = make(map[string]*big.Int)
			}
			m[sym][wallet] = wei
		}
	}
	return m
}

func findTokenAddr(snap *Snapshot, symbol string) string {
	for _, h := range snap.Holders {
		if pos, ok := h.Positions[symbol]; ok {
			return pos.TokenAddress
		}
	}
	return ""
}

func loadManifestAddrs(path string, out map[string]string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var m struct {
		Core        map[string]string `json:"core"`
		BatchTokens map[string]string `json:"batchTokens"`
		Deployed    map[string]string `json:"deployed"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return
	}
	for sym, addr := range m.BatchTokens {
		out[sym] = addr
	}
	for sym, addr := range m.Deployed {
		out[sym] = addr
	}
}

func loadHorseManifestAddrs(path string, out map[string]string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var m struct {
		Deployed map[string]string `json:"deployed"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return
	}
	for sym, addr := range m.Deployed {
		out[sym] = addr
	}
}

func writeMintResults(path string, mints []MintAction, results []MintResult) {
	if path == "" {
		return
	}
	var data interface{}
	if results != nil {
		data = results
	} else {
		data = mints
	}
	b, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "error marshaling results: %v\n", err)
		return
	}
	if err := os.WriteFile(path, b, 0644); err != nil {
		fmt.Fprintf(os.Stderr, "error writing %s: %v\n", path, err)
	}
}

func sortedKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
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

func stripHexPrefix(s string) string {
	return strings.TrimPrefix(strings.TrimPrefix(s, "0x"), "0X")
}

func ensureHexPrefix(s string) string {
	if strings.HasPrefix(s, "0x") || strings.HasPrefix(s, "0X") {
		return s
	}
	return "0x" + s
}

type stringSlice []string

func (s *stringSlice) String() string { return strings.Join(*s, ",") }
func (s *stringSlice) Set(v string) error {
	*s = append(*s, v)
	return nil
}
