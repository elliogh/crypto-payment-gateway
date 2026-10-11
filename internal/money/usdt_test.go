package money

import (
	"math"
	"testing"
)

func TestParseAndFormat(t *testing.T) {
	for _, tc := range []struct {
		input  string
		units  USDT
		output string
	}{
		{input: "0", units: 0, output: "0.000000"},
		{input: "0.000001", units: 1, output: "0.000001"},
		{input: "100", units: 100_000_000, output: "100.000000"},
		{input: "001.20", units: 1_200_000, output: "1.200000"},
		{input: "9223372036854.775807", units: USDT(math.MaxInt64), output: "9223372036854.775807"},
	} {
		t.Run(tc.input, func(t *testing.T) {
			got, err := Parse(tc.input)
			if err != nil || got != tc.units {
				t.Fatalf("Parse: got %v, %v", got, err)
			}
			if got.String() != tc.output {
				t.Fatalf("format: %s", got.String())
			}
		})
	}
	invalidInputs := []string{
		"", "-1", "+1", " 1", "1 ", "1e2", ".1", "1.",
		"1.0000001", "1.2.3", "NaN", "1,2", "١",
		"9223372036854.775808", "999999999999999999999999999999",
	}
	for _, input := range invalidInputs {
		if _, err := Parse(input); err == nil {
			t.Errorf("accepted %q", input)
		}
	}
}

func TestCheckedArithmetic(t *testing.T) {
	if got, err := Add(USDT(math.MaxInt64)-1, 1); err != nil || got != USDT(math.MaxInt64) {
		t.Fatal(got, err)
	}
	for _, pair := range [][2]USDT{{USDT(math.MaxInt64), 1}, {-1, 1}, {1, -1}} {
		if _, err := Add(pair[0], pair[1]); err == nil {
			t.Fatal("accepted overflowing/negative sum")
		}
	}
	for _, tc := range []struct {
		gross USDT
		bps   int
		want  USDT
	}{
		{gross: 100_000_000, bps: 100, want: 1_000_000},
		{gross: 1, bps: 100, want: 0},
		{gross: 999_999, bps: 100, want: 9_999},
		{gross: USDT(math.MaxInt64), bps: 10_000, want: USDT(math.MaxInt64)},
		{gross: 100, bps: 0, want: 0},
	} {
		got, err := Fee(tc.gross, tc.bps)
		if err != nil || got != tc.want {
			t.Fatalf("fee: got %v %v, want %v", got, err, tc.want)
		}
	}
	for _, tc := range []struct {
		gross USDT
		bps   int
	}{
		{gross: -1, bps: 100},
		{gross: 1, bps: -1},
		{gross: 1, bps: 10_001},
	} {
		if _, err := Fee(tc.gross, tc.bps); err == nil {
			t.Fatal("accepted invalid fee")
		}
	}
}
