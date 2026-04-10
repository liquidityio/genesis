package main

import (
	"encoding/hex"
	"encoding/json"
	"math/big"
	"os"
	"testing"
)

func TestKeccak256(t *testing.T) {
	// keccak256("") = c5d2460186f7233c927e7db2dcc703c0e500b653ca82273b7bfad8045d85a470
	hash := keccak256([]byte{})
	expected := "c5d2460186f7233c927e7db2dcc703c0e500b653ca82273b7bfad8045d85a470"
	got := hex.EncodeToString(hash[:])
	if got != expected {
		t.Errorf("keccak256(''): got %s, want %s", got, expected)
	}
}

func TestBalanceOfSlot(t *testing.T) {
	// For address 0x0234...87b5 at mapping slot 0:
	// keccak256(abi.encode(address, uint256(0)))
	slot := balanceOfSlot("0234825373067be82ea7f5f91c4755d6349d87b5", 0)
	if slot == "" {
		t.Fatal("empty slot")
	}
	// Must be a 32-byte hex string with 0x prefix
	if len(slot) != 66 {
		t.Errorf("slot length: got %d, want 66", len(slot))
	}
	if slot[:2] != "0x" {
		t.Error("slot must start with 0x")
	}
}

func TestBalanceOfSlotDeterministic(t *testing.T) {
	// Same input must produce same output
	a := balanceOfSlot("0234825373067be82ea7f5f91c4755d6349d87b5", 0)
	b := balanceOfSlot("0234825373067be82ea7f5f91c4755d6349d87b5", 0)
	if a != b {
		t.Errorf("non-deterministic: %s != %s", a, b)
	}
}

func TestBalanceOfSlotDifferentAddresses(t *testing.T) {
	a := balanceOfSlot("0234825373067be82ea7f5f91c4755d6349d87b5", 0)
	b := balanceOfSlot("047acdf0a1564db38a60603e0f8208e1e6a4a6d9", 0)
	if a == b {
		t.Error("different addresses should produce different slots")
	}
}

func TestToHex32(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"0", "0x0000000000000000000000000000000000000000000000000000000000000000"},
		{"1", "0x0000000000000000000000000000000000000000000000000000000000000001"},
		{"1000000000000000000", "0x0000000000000000000000000000000000000000000000000de0b6b3a7640000"},
	}
	for _, tt := range tests {
		v := new(big.Int)
		v.SetString(tt.input, 10)
		got := toHex32(v)
		if got != tt.expected {
			t.Errorf("toHex32(%s): got %s, want %s", tt.input, got, tt.expected)
		}
	}
}

func TestSlot(t *testing.T) {
	got := slot(2)
	expected := "0x0000000000000000000000000000000000000000000000000000000000000002"
	if got != expected {
		t.Errorf("slot(2): got %s, want %s", got, expected)
	}
}

func TestCREATE2Deterministic(t *testing.T) {
	addr1 := computeCREATE2(deployerAddr, "USDL")
	addr2 := computeCREATE2(deployerAddr, "USDL")
	if addr1 != addr2 {
		t.Errorf("CREATE2 not deterministic: %s != %s", addr1, addr2)
	}
}

func TestCREATE2DifferentSymbols(t *testing.T) {
	a := computeCREATE2(deployerAddr, "USDL")
	b := computeCREATE2(deployerAddr, "BTC")
	if a == b {
		t.Error("different symbols should produce different addresses")
	}
}

func TestCREATE2ValidAddress(t *testing.T) {
	addr := computeCREATE2(deployerAddr, "USDL")
	// Must be 20 bytes = 40 hex chars
	if len(addr) != 40 {
		t.Errorf("address length: got %d, want 40", len(addr))
	}
	// Must be valid hex
	if _, err := hex.DecodeString(addr); err != nil {
		t.Errorf("invalid hex address: %v", err)
	}
}

func TestStripHexPrefix(t *testing.T) {
	tests := []struct {
		input, expected string
	}{
		{"0xabc", "abc"},
		{"0Xabc", "abc"},
		{"abc", "abc"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := stripHexPrefix(tt.input); got != tt.expected {
			t.Errorf("stripHexPrefix(%q): got %q, want %q", tt.input, got, tt.expected)
		}
	}
}

func TestLoadSnapshot(t *testing.T) {
	// Write a minimal test snapshot
	snap := Snapshot{
		Chain: "test",
		Block: 100,
		Holders: []Holder{
			{
				Wallet: "0xabcdef0000000000000000000000000000000001",
				Owner:  "Test User",
				Positions: map[string]Position{
					"USDL": {
						TokenAddress: "0x1111111111111111111111111111111111111111",
						RawWei:       "1000000000000000000",
						Amount:       1.0,
					},
				},
			},
		},
		TotalPos: 1,
	}

	data, _ := json.Marshal(snap)
	f, err := os.CreateTemp("", "snapshot-*.json")
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
	if loaded.Block != 100 {
		t.Errorf("block: got %d, want 100", loaded.Block)
	}
	if len(loaded.Holders) != 1 {
		t.Errorf("holders: got %d, want 1", len(loaded.Holders))
	}
	if loaded.Holders[0].Positions["USDL"].RawWei != "1000000000000000000" {
		t.Error("USDL raw_wei mismatch")
	}
}

func TestAccessControlBaseSlot(t *testing.T) {
	base := accessControlBaseSlot()
	if base.Sign() <= 0 {
		t.Error("base slot should be positive")
	}
	// Should be 32 bytes
	if len(base.Bytes()) > 32 {
		t.Error("base slot exceeds 32 bytes")
	}
}

func TestRoleHasRoleSlot(t *testing.T) {
	base := accessControlBaseSlot()
	deployer := stripHexPrefix(deployerAddr)

	// MINTER_ROLE slot should be deterministic
	a := roleHasRoleSlot(base, minterRoleHash, deployer)
	b := roleHasRoleSlot(base, minterRoleHash, deployer)
	if a != b {
		t.Errorf("role slot not deterministic: %s != %s", a, b)
	}

	// Different roles should produce different slots
	adminSlot := roleHasRoleSlot(base, adminRoleHash, deployer)
	minterSlot := roleHasRoleSlot(base, minterRoleHash, deployer)
	if adminSlot == minterSlot {
		t.Error("admin and minter slots should differ")
	}
}

func TestUniqueWallets(t *testing.T) {
	holders := []Holder{
		{Wallet: "0xAAA"},
		{Wallet: "0xaaa"}, // duplicate (case-insensitive)
		{Wallet: "0xBBB"},
	}
	wallets := uniqueWallets(holders)
	if len(wallets) != 2 {
		t.Errorf("got %d unique wallets, want 2", len(wallets))
	}
}

func TestEndToEnd(t *testing.T) {
	// Create a snapshot, run genesis-alloc logic, verify output
	snap := &Snapshot{
		Chain: "test",
		Block: 42,
		Holders: []Holder{
			{
				Wallet: "0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				Owner:  "Alice",
				Positions: map[string]Position{
					"TOKEN": {
						TokenAddress: "0x1111111111111111111111111111111111111111",
						RawWei:       "5000000000000000000", // 5 tokens
						Amount:       5.0,
					},
				},
			},
			{
				Wallet: "0xbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
				Owner:  "Bob",
				Positions: map[string]Position{
					"TOKEN": {
						TokenAddress: "0x1111111111111111111111111111111111111111",
						RawWei:       "3000000000000000000", // 3 tokens
						Amount:       3.0,
					},
				},
			},
		},
		TotalPos: 2,
	}

	// Build token balances
	type tokenBalance struct {
		wallet string
		wei    *big.Int
	}
	var balances []tokenBalance
	totalSupply := new(big.Int)

	for _, h := range snap.Holders {
		pos := h.Positions["TOKEN"]
		wei := new(big.Int)
		wei.SetString(pos.RawWei, 10)
		balances = append(balances, tokenBalance{wallet: stripHexPrefix(h.Wallet), wei: wei})
		totalSupply.Add(totalSupply, wei)
	}

	// Verify totalSupply = 8 tokens
	expected := new(big.Int)
	expected.SetString("8000000000000000000", 10)
	if totalSupply.Cmp(expected) != 0 {
		t.Errorf("totalSupply: got %s, want %s", totalSupply.String(), expected.String())
	}

	// Verify storage slot for totalSupply at slot 2
	s2 := slot(2)
	if s2 != "0x0000000000000000000000000000000000000000000000000000000000000002" {
		t.Errorf("slot(2) wrong: %s", s2)
	}

	// Verify storage value for totalSupply
	v := toHex32(totalSupply)
	if v != "0x0000000000000000000000000000000000000000000000006f05b59d3b200000" {
		t.Errorf("totalSupply hex wrong: %s", v)
	}

	// Verify Alice's balanceOf slot
	aliceSlot := balanceOfSlot("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", 0)
	bobSlot := balanceOfSlot("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", 0)
	if aliceSlot == bobSlot {
		t.Error("Alice and Bob should have different balanceOf slots")
	}
}
