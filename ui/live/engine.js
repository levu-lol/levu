/* Pricing, aggregation and risk, in the browser.
 *
 * This is a fourth implementation of logic that already exists in Rust (the
 * VM), Go (the control plane) and Solidity (settlement). That duplication is a
 * real cost and it has already bitten this project once: adding a field to the
 * account leaf desynced Go and Solidity from Rust until the tests caught it.
 *
 * It is here because a static page has no server to ask, and every constant
 * below is pinned to its source so drift is at least findable:
 *
 *   RENT           capacity::RentCurve::conservative_default   (perpvm/src/capacity.rs)
 *   HEALTH         health.DefaultConfig                        (internal/health/health.go)
 *   AGG            oracle.DefaultConfig                        (internal/oracle/oracle.go)
 *   DEPTH_2PCT     V3Pool.DepthWithin2Pct                      (internal/indexer/univ3.go)
 *
 * Prices are read from chain. Nothing here is invented.
 */

export const Q96 = 2n ** 96n;
export const E18 = 10n ** 18n;

/* sqrt(1.02) - 1, scaled 1e18. Moving a concentrated-liquidity price by 2%
   consumes L * sqrtP * this / 2^96 of the quote token. */
const SQRT102_MINUS_1 = 9950493836207795n;
/* 1 - 1/sqrt(1.02), scaled 1e18. The token0 counterpart: a 2% move consumes
   L*sqrtP*(sqrt(1.02)-1) of token1 but L/sqrtP*(1-1/sqrt(1.02)) of token0. */
const ONE_MINUS_INV_SQRT102 = 9852457023325691n;

export const RENT = { base: 1e-5, kink: 0.8, below: 5e-5, above: 2e-3 };
export const HEALTH = {
  minVolume: 50_000, minTVL: 250_000, minDepth: 50_000,
  minConfidence: 5_000, maxTopHolderBps: 5_000,
  targetDepth: 1_000_000, targetUnderwriting: 500_000,
  targetAgeHours: 7 * 24, maxVolBps: 40_000,
  wDepth: 1_500, wUnderwriting: 2_000, wOracle: 2_500,
  wMaturity: 1_000, wDispersion: 1_500, wStability: 1_500,
  tiers: [{ min: 8_500, lev: 33 }, { min: 7_000, lev: 20 },
          { min: 4_500, lev: 10 }, { min: 4_000, lev: 1 }],
  oiFractionBps: 5_000, underwritingMultipleBps: 30_000,
  liquidationFeeBps: 100, minMaintenanceBps: 10,
  markBandBps: 50, manipulationSafety: 4, minBookForLeverage: 250_000,
};
export const AGG = {
  maxDeviation: 0.05, minSources: 2, targetSources: 4,
  targetLiquidity: 1_000_000, minSourceLiquidity: 250_000,
};
export const BPS = 10_000;

/* ---- chain ------------------------------------------------------------- */

export const SEL = {
  slot0: "0x3850c7bd",
  liquidity: "0x1a686502",
  token0: "0x0dfe1681",
  decimals: "0x313ce567",
  getPool: "0x1698ee82",
};

function pad32(hex) {
  return hex.replace(/^0x/, "").toLowerCase().padStart(64, "0");
}

export function encodeGetPool(tokenA, tokenB, fee) {
  return SEL.getPool + pad32(tokenA) + pad32(tokenB) + pad32(fee.toString(16));
}

/** One batched JSON-RPC round trip, with a hard deadline.
 *
 *  Batching matters: a tick needs two calls per pool, and sending them
 *  separately multiplies latency by the pool count for no reason.
 *
 *  The timeout matters more. `fetch` has no default deadline, so a hung
 *  connection stalls the poll loop indefinitely — and the page goes on showing
 *  the last price it got, looking live while being frozen. That is the worst
 *  failure mode a price display can have, so every request carries an abort. */
export async function rpcBatch(url, calls, timeoutMs = 6000) {
  if (calls.length === 0) return [];
  const ctl = new AbortController();
  const timer = setTimeout(() => ctl.abort(), timeoutMs);
  try {
    return await rpcBatchInner(url, calls, ctl.signal);
  } finally {
    clearTimeout(timer);
  }
}

async function rpcBatchInner(url, calls, signal) {
  const body = calls.map((c, i) => ({
    jsonrpc: "2.0", id: i + 1, method: "eth_call",
    params: [{ to: c.to, data: c.data }, "latest"],
  }));
  const res = await fetch(url, {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify(body),
    signal,
  });
  if (!res.ok) throw new Error("rpc " + res.status);
  const out = await res.json();
  const arr = Array.isArray(out) ? out : [out];
  const byId = new Map(arr.map((r) => [r.id, r]));
  return calls.map((_, i) => {
    const r = byId.get(i + 1);
    if (!r || r.error || !r.result) return null;
    return r.result;
  });
}

export function word(hex, i) {
  const h = hex.replace(/^0x/, "");
  const s = h.slice(i * 64, (i + 1) * 64);
  return s ? BigInt("0x" + s) : 0n;
}

export function addressFromWord(hex, i) {
  const h = hex.replace(/^0x/, "");
  return "0x" + h.slice(i * 64 + 24, (i + 1) * 64);
}

/* ---- pool maths --------------------------------------------------------- */

/** Quote per base, as a Number.
 *
 *  slot0 reports token1/token0 in raw units, so the decimal difference has to
 *  be applied or an 18/6-decimal pair reads twelve orders of magnitude off. */
export function poolPrice(sqrtP, dec0, dec1, quoteIsToken0) {
  if (sqrtP <= 0n) return 0;
  let p = (sqrtP * sqrtP * E18) / (Q96 * Q96); // token1 per token0, 1e18
  if (quoteIsToken0) {
    if (p === 0n) return 0;
    p = (E18 * E18) / p; // invert to token0 per token1
  }
  const baseDec = quoteIsToken0 ? dec1 : dec0;
  const quoteDec = quoteIsToken0 ? dec0 : dec1;
  const diff = baseDec - quoteDec;
  if (diff > 0) p *= 10n ** BigInt(diff);
  else if (diff < 0) p /= 10n ** BigInt(-diff);
  return Number(p) / 1e18;
}

/** Quote-denominated liquidity executable within ±2%, in whole quote units.
 *
 *  Both sides count, and they are exactly equal: moving the price up consumes
 *  quote, moving it down consumes base worth the same in quote terms. The
 *  haircut is applied because constant liquidity across the band overstates
 *  depth once a tick boundary is crossed, and overstating depth is the
 *  dangerous direction. */
export function poolDepth(L, sqrtP, quoteDec, haircutBps, quoteIsToken0 = false) {
  if (L <= 0n || sqrtP <= 0n) return 0;
  /* Which closed form applies depends on which token the quote is. Using the
     token1 form for a token0 quote overstates by a factor of the raw price --
     2.6e11x on a 6-decimal quote against an 18-decimal base. USDG sorts low
     enough to be token0 in about half the pools on this chain. */
  let d = quoteIsToken0
    ? (2n * L * Q96 * ONE_MINUS_INV_SQRT102) / sqrtP / E18
    : (2n * L * sqrtP * SQRT102_MINUS_1) / Q96 / E18;
  if (haircutBps > 0) d = (d * BigInt(BPS - haircutBps)) / BigInt(BPS);
  return Number(d / 10n ** BigInt(quoteDec));
}

/* ---- aggregation -------------------------------------------------------- */

/** Liquidity-weighted median: the price at which cumulative depth first
 *  reaches half the total. Ties broken by name so the same quotes always
 *  produce the same answer. */
export function weightedMedian(sources) {
  const s = [...sources].sort((a, b) =>
    a.price !== b.price ? a.price - b.price : a.name.localeCompare(b.name));
  const total = s.reduce((t, x) => t + x.liquidity, 0);
  let cum = 0;
  for (const x of s) {
    cum += x.liquidity;
    if (cum >= total / 2) return x.price;
  }
  return s.length ? s[s.length - 1].price : 0;
}

/** Aggregate venue quotes into an index and a confidence.
 *
 *  Confidence is coverage x depth x agreement, and coverage counts *effective*
 *  observations — each weighted by how closely it agrees and whether it carries
 *  real depth. A naive source count is exploitable in both directions: adding a
 *  disagreeing venue would raise it, and empty sybils would manufacture
 *  apparent independence. */
export function aggregate(sources) {
  const out = { price: 0, confidence: 0, coverage: 0, depth: 0, agreement: 0, used: [], rejected: [] };
  const live = sources.filter((s) => s.price > 0 && s.liquidity > 0);
  if (live.length === 0) return out;

  const provisional = weightedMedian(live);
  const kept = [];
  for (const s of live) {
    const dev = Math.abs(s.price - provisional) / provisional;
    if (dev > AGG.maxDeviation) {
      out.rejected.push({ name: s.name, reason: "deviates " + (dev * 100).toFixed(1) + "%" });
    } else kept.push(s);
  }
  if (kept.length === 0) return out;

  out.price = weightedMedian(kept);
  out.used = kept.map((s) => s.name);

  const total = kept.reduce((t, s) => t + s.liquidity, 0);

  let effective = 0, wmad = 0;
  for (const s of kept) {
    const dev = Math.abs(s.price - out.price) / out.price;
    const agree = Math.max(0, 1 - Math.min(1, dev / AGG.maxDeviation));
    const weight = Math.min(1, s.liquidity / AGG.minSourceLiquidity);
    effective += agree * weight;
    wmad += dev * s.liquidity;
  }
  wmad = total > 0 ? wmad / total : 0;

  out.coverage = Math.min(1, effective / AGG.targetSources);
  out.depth = Math.min(1, total / AGG.targetLiquidity);
  out.agreement = 1 - Math.min(1, wmad / AGG.maxDeviation);
  out.confidence = Math.round(out.coverage * out.depth * out.agreement * BPS);
  out.healthy = kept.length >= AGG.minSources;
  return out;
}

/* ---- risk --------------------------------------------------------------- */

function ratio(v, target) {
  if (target <= 0 || v <= 0) return 0;
  return Math.min(BPS, Math.round((v / target) * BPS));
}

/** Score a market. Cheap-to-fake signals are gates; the score is carried by
 *  costly ones. Extreme holder concentration is a gate too, not a discount. */
export function assess(o) {
  const gates = [];
  if (o.volume24h < HEALTH.minVolume) gates.push("24h volume below floor");
  if (o.tvl < HEALTH.minTVL) gates.push("spot TVL below floor");
  if (o.depth < HEALTH.minDepth) gates.push("executable depth below floor");
  if (o.ageHours < 24) gates.push("token too young");
  if (o.confidence < HEALTH.minConfidence) gates.push("oracle confidence below floor");
  if (o.topHolderBps > HEALTH.maxTopHolderBps) gates.push("supply too concentrated");

  const sub = {
    depth: ratio(o.depth, HEALTH.targetDepth),
    underwriting: ratio(o.underwritten, HEALTH.targetUnderwriting),
    oracle: Math.max(0, Math.min(BPS, o.confidence)),
    maturity: ratio(o.ageHours, HEALTH.targetAgeHours),
    dispersion: Math.max(0, BPS - Math.max(o.topHolderBps, o.topPoolBps)),
    stability: Math.max(0, BPS - ratio(o.volBps, HEALTH.maxVolBps)),
  };
  const total = Math.round(
    (sub.depth * HEALTH.wDepth + sub.underwriting * HEALTH.wUnderwriting +
     sub.oracle * HEALTH.wOracle + sub.maturity * HEALTH.wMaturity +
     sub.dispersion * HEALTH.wDispersion + sub.stability * HEALTH.wStability) / BPS);

  return { total, sub, gates, eligible: gates.length === 0 };
}

/** Turn a score into a risk envelope. The cap comes from what can actually be
 *  closed or covered, never from the score. */
export function derive(o, score) {
  if (!score.eligible) return null;
  let lev = 0;
  for (const t of HEALTH.tiers) if (score.total >= t.min) { lev = t.lev; break; }
  if (lev === 0) return null;

  const fromDepth = Math.floor((o.depth * HEALTH.oiFractionBps) / BPS);
  const fromCapital = Math.floor((o.underwritten * HEALTH.underwritingMultipleBps) / BPS);
  const oiCap = fromDepth + fromCapital;

  // Slippage is measured against the book, not total capacity: underwriting
  // capital absorbs losses, it does not fill orders.
  const slippageBps = o.depth > 0 ? Math.floor((oiCap * 200) / o.depth) : 0;
  let maintenance = Math.max(HEALTH.liquidationFeeBps + slippageBps, HEALTH.minMaintenanceBps);
  let initial = Math.floor(BPS / lev);
  while (initial <= maintenance && lev > 1) { lev -= 1; initial = Math.floor(BPS / lev); }
  if (initial <= maintenance) return null;

  return { leverage: lev, oiCap, maintenanceBps: maintenance, initialBps: initial };
}

/** Capacity rent for one interval, as an annualised fraction. */
export function rentAnnual(utilisation) {
  const u = Math.max(0, Math.min(1, utilisation));
  const r = u <= RENT.kink
    ? RENT.base + RENT.below * (u / RENT.kink)
    : RENT.base + RENT.below + RENT.above * ((u - RENT.kink) / (1 - RENT.kink));
  return r * 24 * 365;
}

/** The mark price: the book, clamped to a band around the index. Without the
 *  clamp, two small orders could put the mid anywhere and drag every account's
 *  margin with it. */
export function deriveMark(index, book, bandBps) {
  if (!(index > 0)) return 0;
  if (!(book > 0) || bandBps <= 0) return index;
  const d = index * (bandBps / BPS);
  return Math.max(index - d, Math.min(index + d, book));
}
