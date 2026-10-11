package ledger

import (
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
)

const (
	assetID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	holdID  = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
)

func TestFingerprint(t *testing.T) {
	a := []Entry{{assetID, Debit, 100}, {holdID, Credit, 100}}
	original := append([]Entry(nil), a...)
	b := []Entry{a[1], a[0]}
	ha, err := Fingerprint(a)
	if err != nil {
		t.Fatal(err)
	}
	// The persisted format must remain stable for idempotent replays.
	const expectedFingerprint = "87d4301a93c04d1b9292e9521597f4b9e13a923c4be766d621fa9c6756ad91c1"
	if ha != expectedFingerprint {
		t.Fatalf("fingerprint compatibility changed: %s", ha)
	}
	hb, err := Fingerprint(b)
	if err != nil || ha != hb {
		t.Fatal("order changed fingerprint", err)
	}
	if !reflect.DeepEqual(a, original) {
		t.Fatal("fingerprinting mutated entries")
	}
}

func TestFingerprintEncodingPreservesLargeAmounts(t *testing.T) {
	data, err := fingerprintBytes([]Entry{{assetID, Debit, math.MaxInt64}, {holdID, Credit, math.MaxInt64}})
	if err != nil || !strings.Contains(string(data), `"Amount":"9223372036854.775807"`) {
		t.Fatalf("large amount lost precision: %s, %v", data, err)
	}
}

func TestFingerprintRejectsInvalidEntries(t *testing.T) {
	tests := []struct {
		name    string
		entries []Entry
	}{
		{name: "empty"},
		{name: "one entry", entries: []Entry{{AccountID: assetID, Side: Debit, Amount: 1}}},
		{name: "unbalanced", entries: []Entry{
			{AccountID: assetID, Side: Debit, Amount: 1},
			{AccountID: holdID, Side: Credit, Amount: 2},
		}},
		{name: "zero amount", entries: []Entry{
			{AccountID: assetID, Side: Debit, Amount: 0},
			{AccountID: holdID, Side: Credit, Amount: 0},
		}},
		{name: "unknown side", entries: []Entry{
			{AccountID: assetID, Side: Side("BAD"), Amount: 1},
			{AccountID: holdID, Side: Credit, Amount: 1},
		}},
		{name: "missing account", entries: []Entry{
			{Side: Debit, Amount: 1},
			{AccountID: holdID, Side: Credit, Amount: 1},
		}},
		{name: "negative amount", entries: []Entry{
			{AccountID: assetID, Side: Debit, Amount: -1},
			{AccountID: holdID, Side: Credit, Amount: -1},
		}},
		{name: "uppercase account", entries: []Entry{
			{AccountID: strings.ToUpper(assetID), Side: Debit, Amount: 1},
			{AccountID: holdID, Side: Credit, Amount: 1},
		}},
		{name: "malformed account", entries: []Entry{
			{AccountID: "asset", Side: Debit, Amount: 1},
			{AccountID: holdID, Side: Credit, Amount: 1},
		}},
		{name: "account without hyphens", entries: []Entry{
			{AccountID: strings.ReplaceAll(assetID, "-", ""), Side: Debit, Amount: 1},
			{AccountID: holdID, Side: Credit, Amount: 1},
		}},
		{name: "account URN", entries: []Entry{
			{AccountID: "urn:uuid:" + assetID, Side: Debit, Amount: 1},
			{AccountID: holdID, Side: Credit, Amount: 1},
		}},
		{name: "bracketed account", entries: []Entry{
			{AccountID: "{" + assetID + "}", Side: Debit, Amount: 1},
			{AccountID: holdID, Side: Credit, Amount: 1},
		}},
		{name: "overflow total", entries: []Entry{
			{AccountID: assetID, Side: Debit, Amount: math.MaxInt64},
			{AccountID: assetID, Side: Debit, Amount: 1},
			{AccountID: holdID, Side: Credit, Amount: math.MaxInt64},
			{AccountID: holdID, Side: Credit, Amount: 1},
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Fingerprint(tc.entries); !errors.Is(err, ErrEntry) {
				t.Fatal("accepted invalid journal", tc.entries)
			}
		})
	}
}
