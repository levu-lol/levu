# census

How many markets on a chain could we actually list, and at what leverage?

Runs the real `health.Assess` / `health.Derive` over observations measured from
chain, and prints the funnel -- which condition eliminated how many -- rather
than a single verdict.

## Measuring Robinhood Chain

    # 1. every PoolCreated, filtered to pools quoted in USDG or WETH
    # 2. 24h of Swap logs -> per-market volume, and which pools are alive
    # 3. slot0 / liquidity / both balances for markets that clear the volume gate
    # 4. 24 stratified windows of Swap logs -> annualised realised volatility
    go run ./cmd/census -in census.robinhood.json -underwriting 500000 -book 250000

`census.robinhood.json` is a snapshot taken 2026-09-04 at block 53,741,541.

## What is measured and what is assumed

Measured from chain: TVL, executable depth within 2%, 24h volume, age, share of
depth in the deepest pool, realised volatility, market cap, oracle confidence.

Three separate questions, and the tool keeps them apart because conflating them
is how a good market gets refused for the wrong reason:

  is there an asset here   the gates. 57 of 238 measured markets clear them.
                           This is the spot-listable set, and nothing we do or
                           fail to do changes it.

  may we lend against it   the score and the leverage blockers. Needs a book of
                           our own (MinBookForLeverage) and a price we can trust.
                           Currently 2 markets, and that does not move with the
                           book: oracle confidence withdraws leverage from 55 of
                           the 57, because the single-source penalty is quadratic
                           in the same liquidity.

  will an order fill       our resting depth at the moment of the trade. Not a
                           listing question at all -- the matching engine's,
                           when someone actually trades.

Not measurable from a public RPC, and passed as flags rather than defaulted:

  -underwriting  first-loss capital bonded behind a lane. Extends how much
                 leveraged exposure the lane may carry. It does not gate spot
                 listing: a fully collateralised position has no loss for it
                 to absorb.
  -book          resting depth we would quote. Below MinBookForLeverage the
                 lane is spot-only -- still listed, still tradeable, 1x.
  -holder-bps    supply concentration. Needs a holder index. Left at zero it is
                 unknown, which the engine scores as the worst case -- so the
                 leverage figures this prints are a floor, not an estimate.

                                  spot-listable   leverable
                 book $0                     57           0
                 book $250k                  57           2
                 book $1m                    57           2

Pools with zero liquidity are excluded from TVL, depth and price: a drained pool
sits at a tick boundary, and dust valued at a boundary price produced TVLs of
1e50 before they were filtered out. Their 24h volume still counts -- they traded,
they just cannot be traded against now.
