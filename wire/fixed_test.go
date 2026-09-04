package wire

import (
	"math/big"
	"testing"
)

func TestFixedRoundTripsThroughBigInt(t *testing.T) {
	cases := []int64{0, 1, -1, 1000, -1000, 1 << 40, -(1 << 40)}
	for _, c := range cases {
		f := FixedWhole(c)
		got := new(big.Int).Quo(f.BigInt(), wad).Int64()
		if got != c {
			t.Errorf("FixedWhole(%d) round-tripped to %d", c, got)
		}
	}
}

func TestFixedArithmetic(t *testing.T) {
	seven := FixedWhole(7)
	six := FixedWhole(6)
	if got := seven.Mul(six); got.Cmp(FixedWhole(42)) != 0 {
		t.Errorf("7*6 = %s, want 42", got)
	}
	if got := FixedWhole(100).Div(FixedWhole(8)); got.String() != "12.5" {
		t.Errorf("100/8 = %s, want 12.5", got)
	}
	if got := seven.Add(six); got.Cmp(FixedWhole(13)) != 0 {
		t.Errorf("7+6 = %s", got)
	}
	if got := six.Sub(seven); got.Cmp(FixedWhole(-1)) != 0 {
		t.Errorf("6-7 = %s", got)
	}
}

// Must match the VM: truncation toward zero, so shorts and longs round the
// same way and neither side leaks value to the other.
func TestRoundingIsSignSymmetric(t *testing.T) {
	third := FixedOne().Div(FixedWhole(3))
	negThird := FixedWhole(-1).Div(FixedWhole(3))
	if third.String() != "0.333333333333333333" {
		t.Errorf("1/3 = %s", third)
	}
	if negThird.Cmp(third.Neg()) != 0 {
		t.Errorf("-1/3 = %s, want %s", negThird, third.Neg())
	}
}

func TestDivideByZeroReturnsZeroRatherThanPanicking(t *testing.T) {
	if got := FixedOne().Div(FixedZero()); !got.IsZero() {
		t.Errorf("1/0 = %s, want 0", got)
	}
	if got := FixedOne().MulDiv(FixedOne(), FixedZero()); !got.IsZero() {
		t.Errorf("muldiv by zero = %s", got)
	}
}

// A signed big-endian value cannot be ordered by raw bytes: the sign bit
// inverts the comparison. This is the bug the high-bit flip in Cmp prevents.
func TestCmpOrdersAcrossZeroCorrectly(t *testing.T) {
	neg := FixedWhole(-5)
	pos := FixedWhole(5)
	zero := FixedZero()
	if neg.Cmp(pos) >= 0 {
		t.Error("-5 must be less than 5")
	}
	if neg.Cmp(zero) >= 0 {
		t.Error("-5 must be less than 0")
	}
	if pos.Cmp(zero) <= 0 {
		t.Error("5 must be greater than 0")
	}
	if neg.Cmp(neg) != 0 {
		t.Error("-5 must equal itself")
	}
	// Raw byte comparison would get this backwards.
	if neg[0] <= pos[0] {
		t.Skip("byte layout changed; the point of Cmp is that this is not the test")
	}
}

func TestMulDivKeepsPrecisionChainingLoses(t *testing.T) {
	chained := FixedOne().Div(FixedWhole(3)).Mul(FixedWhole(3))
	if chained.Cmp(FixedOne()) == 0 {
		t.Error("chained div/mul should lose a unit; test premise wrong")
	}
	exact := FixedOne().MulDiv(FixedWhole(3), FixedWhole(3))
	if exact.Cmp(FixedOne()) != 0 {
		t.Errorf("mul_div = %s, want 1", exact)
	}
}

// Memecoin magnitudes: eight leading zeros against a quantity in the hundreds
// of millions. This is the case that overflows a naive i128 intermediate.
func TestMemecoinNotional(t *testing.T) {
	price := FixedRawInt64(12_310_000_000_000) // 0.00001231
	qty := FixedWhole(300_000_000)
	if got := price.Mul(qty).String(); got != "3693.0" {
		t.Errorf("notional = %s, want 3693.0", got)
	}
}
