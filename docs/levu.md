# Levu

A perpetual futures exchange where the leverage on a market is derived from that
market's measured liquidity, and moves when the liquidity moves.

It runs on [Roda](architecture.md) — one lane per market.

---

## The idea

Every perp venue picks a number. This asset gets 20×, that one gets 5×, and the
number stays where it was put until someone remembers to move it. It is a
judgement made once, usually at listing, usually when the market looked its best.

Levu derives it instead. Leverage is a consequence of how cheaply a position can
be closed:

```
maxLeverage = 10000 / (buffer × maintenance)
```

and `maintenance` comes from what closing actually costs against the book that
would have to absorb it. A market deep enough for single-digit-bps maintenance
supports leverage in the hundreds; one that costs 3% to close supports about
fifteen. There is no tier table in that sentence. The ladder in the config is a
*cap* on what a score has earned, never the source of the number.

The consequence traders feel: **leverage falls as your own position grows.** The
market-wide figure is what the largest permitted position gets. Ask for more
size than the book can carry and the offer shrinks to what the book can carry,
rather than being granted and discovered later.

## The ladder

A market is not listed or unlisted. It occupies a rung, and it moves.

```
not listed → spot (1×) → leverage, scaled by depth → back to spot → reduce only → closed
```

**Gates** decide whether there is an asset here at all: 24h volume, spot TVL,
age, supply concentration. Fail one and there is no market at any score.

**Leverage blockers** are different, and keeping them separate matters. Venue
depth below a floor, or an oracle we cannot trust, means the market is real and
tradeable but must not be levered. It drops to the spot rung; it does not close.

**Score** decides how far above 1× a market climbs, from depth, underwriting,
oracle confidence, maturity, dispersion and volatility.

**Our own book** decides whether it climbs at all. Below `MinBookForLeverage`
there is nothing to close a liquidation into, so the lane offers 1× however good
the asset is. This is the distinction that took longest to get right: the depth
of a Uniswap pool is not depth we can lend against. Venue depth bounds the cost
of *faking the price*; our own resting book bounds what a *forced close can
eat*. Sizing exposure from the first while closing against the second is how a
liquidation becomes a no-op that reports success.

The spot rung exists so a fading market does not become a cliff. A lane whose
score drifts below the lowest leverage tier goes to 1× rather than to accepting
nothing, because refusing every new order forces every holder toward the exit at
once, on a book that is already thinner than it was.

## What is measured, on Robinhood Chain

A full census at block 53,741,541: 426,471 pools, 422,208 tokens with a USDG or
WETH pool, 3,691 markets that traded in 24 hours, $1.33bn of volume.

```
  250  clear the $50k volume gate
  238  still hold liquidity
   57  clear every gate          ← the listable set
```

Fifty-seven, and that number does not move with our balance sheet — it is a
property of the chain. What our capital and our book decide is what those 57 are
*offered*: with no book, all 57 trade at 1×; with a book, the ones whose price we
can trust climb.

The listable set is almost entirely tokenised equities — NVDA, GLD, QQQ, SPY,
LLY, RBLX, GME — plus WETH and the memecoins with real depth, PONS and CASHCAT,
both ETH-quoted.

Leverage today is limited by neither capital nor book. Oracle confidence
withdraws it from 55 of the 57, because coverage and depth are both derived from
the same liquidity and multiply, and almost everything on this chain trades on a
single venue.

## Trading it

- **[levu.lol/trade](https://levu.lol/trade)** — paper trading, all 57 markets,
  live prices read from chain in the browser. Fills are simulated against each
  pool's own executable depth using the same linear model the risk engine uses,
  and the ticket refuses a size past a quarter of the book rather than quoting a
  number it cannot stand behind.
- **[levu.lol/terminal](https://levu.lol/terminal)** — one market in depth:
  venue aggregation, oracle confidence, executable depth, the risk envelope.

Collateral for paper trading is **tUSDG**, native to our layer and bridged to
nothing. A faucet mints 1,000 a day per identity. It is deliberately not called
USDG: a paper balance wearing a real asset's ticker is one screenshot away from
being mistaken for it, and when a bridge does exist the two must not have been
sharing a name.

## What is not built

- No bridge. tUSDG is native; collateral cannot come from Robinhood Chain yet.
- Nothing deployed to a public chain.
- The health engine's thresholds are judgement, not fitted. The calibration
  pipeline is complete end to end; what is missing is labelled history, and no
  amount of code supplies that.
- Supply concentration is asserted, not measured — it needs a holder index, and
  a public RPC will not give one. Unknown is scored as the worst case, so the
  figures are a floor rather than an estimate.
- Cross-market correlation is not modelled. Each lane is assessed alone.
