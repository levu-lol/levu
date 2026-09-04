package wire

import (
	"math"
	"math/big"
)

// WAD is the fixed-point scale shared with the VM and with Solidity: 1e18.
const WADDecimals = 18

var wad = new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)

// WadInt returns a copy of the scale factor, for callers converting a Fixed
// into a plain integer.
func WadInt() *big.Int { return new(big.Int).Set(wad) }

// Fixed is a WAD-scaled signed 128-bit value in big-endian byte order.
//
// The control plane deliberately does not do arithmetic on these. Money math
// belongs to the VM, where it is checked and provable; Go's job is to order
// intents and move bytes. What Go needs is construction, ordering and display,
// which is all this provides.
type Fixed [16]byte

// FixedWhole builds n.0 — that is, n * 1e18.
func FixedWhole(n int64) Fixed {
	v := new(big.Int).Mul(big.NewInt(n), wad)
	return fixedFromBig(v)
}

// FixedRaw builds from an already-scaled integer.
func FixedRaw(v *big.Int) Fixed {
	return fixedFromBig(v)
}

// FixedRawInt64 builds from an already-scaled int64, for small values such as
// fee rates expressed in WAD.
// FixedFloat converts a float to WAD fixed point.
//
// For prices and quantities that arrive from outside -- an oracle reading, a
// browser's order ticket -- where the value is already a float and rounding it
// once here is honest. Never for anything derived from committed state: the VM
// works in exact fixed point and a round trip through float64 loses precision
// that the state root will notice.
func FixedFloat(v float64) Fixed {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return Fixed{}
	}
	f := new(big.Float).SetFloat64(v)
	f.Mul(f, new(big.Float).SetInt(wad))
	n, _ := f.Int(nil)
	return FixedRaw(n)
}

func FixedRawInt64(v int64) Fixed {
	return fixedFromBig(big.NewInt(v))
}

var zeroFixed Fixed

// FixedZero is the additive identity.
func FixedZero() Fixed { return zeroFixed }

func fixedFromBig(v *big.Int) Fixed {
	var f Fixed
	neg := v.Sign() < 0
	mag := new(big.Int).Abs(v)
	b := mag.Bytes()
	if len(b) > 16 {
		panic("wire: value does not fit in i128")
	}
	copy(f[16-len(b):], b)
	if neg {
		// Two's complement: invert and add one.
		var carry uint16 = 1
		for i := 15; i >= 0; i-- {
			s := uint16(^f[i]) + carry
			f[i] = byte(s)
			carry = s >> 8
		}
	}
	return f
}

// BigInt returns the raw scaled value.
func (f Fixed) BigInt() *big.Int {
	negative := f[0]&0x80 != 0
	b := f
	if negative {
		var carry uint16 = 1
		for i := 15; i >= 0; i-- {
			s := uint16(^b[i]) + carry
			b[i] = byte(s)
			carry = s >> 8
		}
	}
	v := new(big.Int).SetBytes(b[:])
	if negative {
		v.Neg(v)
	}
	return v
}

// Cmp orders two Fixed values.
//
// A signed big-endian value cannot be compared as raw bytes, because the sign
// bit inverts the ordering. Flipping the high bit maps the signed range onto
// the unsigned one so lexicographic comparison becomes correct.
func (f Fixed) Cmp(o Fixed) int {
	a, b := f, o
	a[0] ^= 0x80
	b[0] ^= 0x80
	for i := 0; i < 16; i++ {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

func (f Fixed) IsZero() bool { return f == zeroFixed }

// ---------------------------------------------------------------------------
// Arithmetic
//
// The control plane does not do *money* math — position, margin and PnL belong
// to the VM, where they are checked and provable. These exist for the oracle
// aggregator and the health engine, whose outputs are VM *inputs* recorded in
// the intent stream. Replay reads the recorded value, so these need not match
// Rust bit for bit; they must only be deterministic in Go.
//
// Truncation is toward zero (big.Int Quo), matching the VM's sign-symmetric
// rounding, so the two agree anyway on everything they both compute.
// ---------------------------------------------------------------------------

func (f Fixed) Add(o Fixed) Fixed {
	return fixedFromBig(new(big.Int).Add(f.BigInt(), o.BigInt()))
}

func (f Fixed) Sub(o Fixed) Fixed {
	return fixedFromBig(new(big.Int).Sub(f.BigInt(), o.BigInt()))
}

// Mul computes f * o, keeping full precision in the intermediate.
func (f Fixed) Mul(o Fixed) Fixed {
	p := new(big.Int).Mul(f.BigInt(), o.BigInt())
	return fixedFromBig(p.Quo(p, wad))
}

// Div computes f / o. Dividing by zero returns zero, which callers must guard
// against themselves — there is no sentinel that would be safe to propagate.
func (f Fixed) Div(o Fixed) Fixed {
	d := o.BigInt()
	if d.Sign() == 0 {
		return zeroFixed
	}
	n := new(big.Int).Mul(f.BigInt(), wad)
	return fixedFromBig(n.Quo(n, d))
}

// MulDiv computes f * num / den in one step, without an intermediate rounding.
func (f Fixed) MulDiv(num, den Fixed) Fixed {
	d := den.BigInt()
	if d.Sign() == 0 {
		return zeroFixed
	}
	n := new(big.Int).Mul(f.BigInt(), num.BigInt())
	return fixedFromBig(n.Quo(n, d))
}

func (f Fixed) Abs() Fixed {
	return fixedFromBig(new(big.Int).Abs(f.BigInt()))
}

func (f Fixed) Neg() Fixed {
	return fixedFromBig(new(big.Int).Neg(f.BigInt()))
}

func (f Fixed) Sign() int { return f.BigInt().Sign() }

// Whole truncates to whole units, for the health engine, which works in whole
// quote units rather than WAD.
//
// Saturates rather than wrapping: a depth reading that silently became negative
// through overflow would read as "no liquidity", which is the one answer that
// must never be produced by accident.
func (f Fixed) Whole() int64 {
	q := new(big.Int).Quo(f.BigInt(), wad)
	if !q.IsInt64() {
		if q.Sign() < 0 {
			return math.MinInt64
		}
		return math.MaxInt64
	}
	return q.Int64()
}

func (f Fixed) IsPositive() bool { return f.Sign() > 0 }

func (f Fixed) Min(o Fixed) Fixed {
	if f.Cmp(o) <= 0 {
		return f
	}
	return o
}

func (f Fixed) Max(o Fixed) Fixed {
	if f.Cmp(o) >= 0 {
		return f
	}
	return o
}

// FixedOne is 1.0.
func FixedOne() Fixed { return FixedWhole(1) }

// String renders the value as a decimal, for logs only.
func (f Fixed) String() string {
	v := f.BigInt()
	neg := v.Sign() < 0
	mag := new(big.Int).Abs(v)
	whole, frac := new(big.Int).QuoRem(mag, wad, new(big.Int))
	s := whole.String()
	fs := frac.String()
	for len(fs) < 18 {
		fs = "0" + fs
	}
	for len(fs) > 1 && fs[len(fs)-1] == '0' {
		fs = fs[:len(fs)-1]
	}
	if neg {
		return "-" + s + "." + fs
	}
	return s + "." + fs
}
