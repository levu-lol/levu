package indexer

import (
	"math"
	"math/big"
	"testing"
)

// The 2% estimate must be in quote units whichever token the quote is.
//
// Regression for a units bug that survived because the reserve bounds hid it:
// the token1 closed form was used unconditionally, so a token0 quote came out
// a factor of the raw price too large and then saturated to roughly TVL. USDG
// sorts low enough to be token0 in about half the pools on this chain.
func TestDepthIsInQuoteUnitsForEitherOrientation(t *testing.T) {
	for _, tc := range []struct {
		name       string
		quoteIs0   bool
		d0, d1     uint8
		usdgPerOne float64 // quote per base, in whole units
	}{
		{"quote is token0", true, 6, 18, 2},
		{"quote is token1", false, 18, 6, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bDec, qDec := tc.d1, tc.d0
			if !tc.quoteIs0 {
				bDec, qDec = tc.d0, tc.d1
			}
			// raw token1 per raw token0
			var praw float64
			if tc.quoteIs0 {
				praw = math.Pow(10, float64(bDec)) / (tc.usdgPerOne * math.Pow(10, float64(qDec)))
			} else {
				praw = tc.usdgPerOne * math.Pow(10, float64(qDec)) / math.Pow(10, float64(bDec))
			}
			sqrtP := math.Sqrt(praw)
			sq, _ := new(big.Float).Mul(big.NewFloat(sqrtP), new(big.Float).SetInt(two96)).Int(nil)

			L, _ := new(big.Int).SetString("1000000000000000000000", 10)
			huge, _ := new(big.Int).SetString("100000000000000000000000000000000000", 10)

			p := V3Pool{QuoteIsToken0: tc.quoteIs0, Decimals0: tc.d0, Decimals1: tc.d1}
			s := &V3State{SqrtPriceX96: sq, Liquidity: L, Balance0: huge, Balance1: huge}

			gotf, _ := new(big.Float).SetInt(p.DepthWithin2Pct(s)).Float64()

			Lf, _ := new(big.Float).SetInt(L).Float64()
			var rawSide float64
			if tc.quoteIs0 {
				rawSide = Lf * (1/sqrtP - 1/(sqrtP*math.Sqrt(1.02)))
			} else {
				rawSide = Lf * sqrtP * (math.Sqrt(1.02) - 1)
			}
			want := 2 * rawSide / math.Pow(10, float64(qDec))

			if r := gotf / want; r < 0.99 || r > 1.01 {
				t.Fatalf("depth %.2f, closed form %.2f: off by %.4gx", gotf, want, r)
			}
		})
	}
}
