# Levu

A perpetual futures exchange that derives each market's maximum leverage from
that market's own measured liquidity, so it moves when the book moves instead of
sitting at whatever number was typed at listing.

**[levu.lol/trade](https://levu.lol/trade)** — paper trade 57 markets on
Robinhood Chain, live prices read from chain in the browser.
**[levu.lol/terminal](https://levu.lol/terminal)** — one market in depth.

[docs/levu.md](docs/levu.md) is the full description: the ladder markets climb
and fall, what the census found, and what is not built.

## What is here

This repository is the exchange: the risk engine and everything that decides
what a market may be offered. The execution layer it runs on lives separately.

| package | |
|---|---|
| `health` | the promotion ladder — gates, leverage blockers, scoring, the risk envelope |
| `oracle` | venue aggregation and the confidence a mark is given |
| `indexer` | Uniswap v3 state: executable depth, price, TVL, orientation |
| `margin` | one balance allocated across the lanes a trader is actually in |
| `mm`, `solver` | quoting and unwind |
| `wire` | the protocol between the exchange and its execution layer |
| `ui` | the site, the paper terminal and the lane terminal |

```sh
go test ./...
go run ./cmd/census -in census.robinhood.json -underwriting 500000 -book 250000
```

`cmd/census` is the interesting one. It runs the real engine over measurements
taken from Robinhood Chain and prints the funnel — which condition eliminated
how many — rather than a single verdict.

## The idea, briefly

Leverage is a consequence of how cheaply a position can be closed:

```
maxLeverage = 10000 / (buffer × maintenance)
```

where `maintenance` comes from what closing actually costs against the book that
would absorb it. There is no tier table in that sentence; the ladder in the
config is a *cap* on what a market has earned, never the source of the number.

The consequence a trader feels: leverage falls as your own position grows. Ask
for more size than the book can carry and the offer shrinks to what it can carry,
rather than being granted and discovered at liquidation.

## Measured, not claimed

Every figure in the docs came from a census of Robinhood Chain mainnet at block
53,741,541: 426,471 pools, 3,691 markets that traded in 24 hours, and 57 that
clear every listing gate. That set is a property of the chain, not of our
balance sheet — it does not move when we add capital.
