package main

import (
	"encoding/json"
	"math/big"
	"os"
	"testing"
)

func TestBuildBalanceMap(t *testing.T) {
	snap := &Snapshot{
		Holders: []Holder{
			{
				Wallet: "0xAAAA",
				Positions: map[string]Position{
					"USDL": {RawWei: "1000000000000000000"},
					"BTC":  {RawWei: "500000000000000000"},
				},
			},
			{
				Wallet: "0xBBBB",
				Positions: map[string]Position{
					"USDL": {RawWei: "2000000000000000000"},
				},
			},
		},
	}

	bals := buildBalanceMap(snap)

	if len(bals) != 2 {
		t.Errorf("expected 2 tokens, got %d", len(bals))
	}

	usdl := bals["USDL"]
	if len(usdl) != 2 {
		t.Errorf("USDL: expected 2 holders, got %d", len(usdl))
	}

	expected := new(big.Int)
	expected.SetString("1000000000000000000", 10)
	if usdl["0xaaaa"].Cmp(expected) != 0 {
		t.Errorf("Alice USDL: got %s, want %s", usdl["0xaaaa"], expected)
	}
}

func TestBuildBalanceMapSkipsZero(t *testing.T) {
	snap := &Snapshot{
		Holders: []Holder{
			{
				Wallet: "0xAAAA",
				Positions: map[string]Position{
					"USDL": {RawWei: "0"},
				},
			},
		},
	}

	bals := buildBalanceMap(snap)
	if len(bals["USDL"]) != 0 {
		t.Error("should skip zero balances")
	}
}

func TestDeltaComputation(t *testing.T) {
	prev := &Snapshot{
		Block: 100,
		Holders: []Holder{
			{
				Wallet: "0xAAAA",
				Positions: map[string]Position{
					"USDL": {RawWei: "1000000000000000000", TokenAddress: "0x1111"},
				},
			},
			{
				Wallet: "0xBBBB",
				Positions: map[string]Position{
					"USDL": {RawWei: "2000000000000000000", TokenAddress: "0x1111"},
				},
			},
		},
	}

	curr := &Snapshot{
		Block: 200,
		Holders: []Holder{
			{
				Wallet: "0xAAAA",
				Positions: map[string]Position{
					// Increased by 500
					"USDL": {RawWei: "1500000000000000000", TokenAddress: "0x1111"},
				},
			},
			{
				Wallet: "0xBBBB",
				Positions: map[string]Position{
					// Decreased by 500 (should be ignored)
					"USDL": {RawWei: "1500000000000000000", TokenAddress: "0x1111"},
				},
			},
			{
				Wallet: "0xCCCC",
				Positions: map[string]Position{
					// Brand new position
					"USDL": {RawWei: "3000000000000000000", TokenAddress: "0x1111"},
				},
			},
		},
	}

	prevBals := buildBalanceMap(prev)
	currBals := buildBalanceMap(curr)

	type mint struct {
		wallet string
		amount *big.Int
		reason string
	}
	var mints []mint

	for sym, currWallets := range currBals {
		prevWallets := prevBals[sym]
		if prevWallets == nil {
			prevWallets = make(map[string]*big.Int)
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
			reason := "increased"
			if prevBal.Sign() == 0 {
				reason = "new"
			}
			_ = sym
			mints = append(mints, mint{wallet: wallet, amount: delta, reason: reason})
		}
	}

	if len(mints) != 2 {
		t.Fatalf("expected 2 mints, got %d", len(mints))
	}

	// Find Alice's mint (increased)
	var aliceMint, carolMint *mint
	for i := range mints {
		switch mints[i].wallet {
		case "0xaaaa":
			aliceMint = &mints[i]
		case "0xcccc":
			carolMint = &mints[i]
		}
	}

	if aliceMint == nil {
		t.Fatal("Alice's mint not found")
	}
	expectedDelta := new(big.Int)
	expectedDelta.SetString("500000000000000000", 10)
	if aliceMint.amount.Cmp(expectedDelta) != 0 {
		t.Errorf("Alice delta: got %s, want %s", aliceMint.amount, expectedDelta)
	}
	if aliceMint.reason != "increased" {
		t.Errorf("Alice reason: got %s, want increased", aliceMint.reason)
	}

	if carolMint == nil {
		t.Fatal("Carol's mint not found")
	}
	carolExpected := new(big.Int)
	carolExpected.SetString("3000000000000000000", 10)
	if carolMint.amount.Cmp(carolExpected) != 0 {
		t.Errorf("Carol delta: got %s, want %s", carolMint.amount, carolExpected)
	}
	if carolMint.reason != "new" {
		t.Errorf("Carol reason: got %s, want new", carolMint.reason)
	}
}

func TestLoadSnapshotRoundtrip(t *testing.T) {
	snap := Snapshot{
		Chain: "avalanche",
		Block: 82552152,
		Holders: []Holder{
			{
				Wallet: "0xabc",
				Owner:  "Test",
				Positions: map[string]Position{
					"USDL": {
						TokenAddress: "0xdef",
						RawWei:       "1000",
						Amount:       0.000000000000001,
					},
				},
			},
		},
	}

	data, _ := json.Marshal(snap)
	f, err := os.CreateTemp("", "snap-*.json")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	f.Write(data)
	f.Close()

	loaded, err := loadSnapshot(f.Name())
	if err != nil {
		t.Fatalf("loadSnapshot: %v", err)
	}
	if loaded.Block != snap.Block {
		t.Errorf("block: got %d, want %d", loaded.Block, snap.Block)
	}
	if loaded.Holders[0].Positions["USDL"].RawWei != "1000" {
		t.Error("raw_wei mismatch")
	}
}

func TestFindTokenAddr(t *testing.T) {
	snap := &Snapshot{
		Holders: []Holder{
			{
				Wallet: "0xaaa",
				Positions: map[string]Position{
					"BTC": {TokenAddress: "0x1234"},
				},
			},
		},
	}

	addr := findTokenAddr(snap, "BTC")
	if addr != "0x1234" {
		t.Errorf("got %s, want 0x1234", addr)
	}

	addr = findTokenAddr(snap, "MISSING")
	if addr != "" {
		t.Errorf("got %s, want empty", addr)
	}
}

func TestMonotonicallyIncreasingOnly(t *testing.T) {
	// Verify that decreased positions produce zero mints
	prev := &Snapshot{
		Block: 100,
		Holders: []Holder{
			{
				Wallet: "0xAAAA",
				Positions: map[string]Position{
					"BTC": {RawWei: "10000000000000000000"},
				},
			},
		},
	}

	curr := &Snapshot{
		Block: 200,
		Holders: []Holder{
			{
				Wallet: "0xAAAA",
				Positions: map[string]Position{
					// Decreased from 10 to 5 -- should NOT mint
					"BTC": {RawWei: "5000000000000000000"},
				},
			},
		},
	}

	prevBals := buildBalanceMap(prev)
	currBals := buildBalanceMap(curr)

	mintCount := 0
	for sym, currWallets := range currBals {
		prevWallets := prevBals[sym]
		if prevWallets == nil {
			prevWallets = make(map[string]*big.Int)
		}
		for wallet, currBal := range currWallets {
			prevBal := prevWallets[wallet]
			if prevBal == nil {
				prevBal = big.NewInt(0)
			}
			if currBal.Cmp(prevBal) > 0 {
				mintCount++
			}
			_ = wallet
		}
	}

	if mintCount != 0 {
		t.Errorf("decreased position should produce 0 mints, got %d", mintCount)
	}
}

func TestWriteMintResults(t *testing.T) {
	f, err := os.CreateTemp("", "results-*.json")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	defer os.Remove(f.Name())

	mints := []MintAction{
		{
			Symbol:    "USDL",
			Recipient: "0xaaa",
			Amount:    big.NewInt(1000),
			Reason:    "new_position",
		},
	}

	writeMintResults(f.Name(), mints, nil)

	data, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}

	var loaded []MintAction
	if err := json.Unmarshal(data, &loaded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(loaded) != 1 {
		t.Errorf("got %d results, want 1", len(loaded))
	}
	if loaded[0].Symbol != "USDL" {
		t.Errorf("symbol: got %s, want USDL", loaded[0].Symbol)
	}
}
