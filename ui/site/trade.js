/* Paper trading against live Robinhood Chain prices.
 *
 * The market list is not assembled here. It is emitted by cmd/census, which
 * runs the real health.Assess and health.Derive over chain measurements, so
 * every market on this page is one the engine actually cleared and carries the
 * engine's own verdict. Nothing about listing is decided in the browser.
 *
 * What the browser does: read each pool's price, simulate a fill against that
 * pool's executable depth, and keep the resulting book in localStorage.
 */
import { rpcBatch, word, poolPrice, poolDepth, SEL } from "/engine.js";

const RPC = "https://rpc.mainnet.chain.robinhood.com";
/* The deepest WETH/USDG pool. ETH-quoted markets are converted through it so a
   single account can hold both kinds without the user doing FX in their head. */
const ETH_POOL = { addr: "0x52e65b17fb6e5ba00ed806f37afcd2daa50271ca",
                   dec0: 18, dec1: 6, quoteIsToken0: false };
const POLL_MS = 12000;
/* The endpoint answers 20 eth_calls in a batch and returns 429 for 58, so a
   tick is split rather than sent whole. Measured, not guessed: n=20 gives 20
   results, n=58 gives HTTP 429. */
const BATCH = 20;
const BATCH_GAP_MS = 180;
const START_CASH = 100_000;
const KEY = "levu.paper.v1";
/* Constant liquidity across the band overstates depth once a tick boundary is
   crossed, and overstating is the dangerous direction. Same haircut the
   terminal uses. */
const HAIRCUT_BPS = 1000;

const $ = (id) => document.getElementById(id);
const fmtUsd = (v, dp = 2) =>
  (v < 0 ? "-" : "") + "$" + Math.abs(v).toLocaleString("en-US",
    { minimumFractionDigits: dp, maximumFractionDigits: dp });
const fmtCompact = (v) => v >= 1e9 ? "$" + (v / 1e9).toFixed(2) + "bn"
  : v >= 1e6 ? "$" + (v / 1e6).toFixed(2) + "m"
  : v >= 1e3 ? "$" + (v / 1e3).toFixed(1) + "k" : "$" + v.toFixed(0);
const fmtPx = (v) => v === 0 ? "—"
  : v >= 1000 ? v.toLocaleString("en-US", { maximumFractionDigits: 2 })
  : v >= 1 ? v.toFixed(4) : v >= 0.0001 ? v.toFixed(6) : v.toExponential(3);
const fmtQty = (v) => Math.abs(v) >= 1000 ? v.toLocaleString("en-US", { maximumFractionDigits: 2 })
  : Math.abs(v) >= 1 ? v.toFixed(4) : v.toFixed(8);

/* ---- state -------------------------------------------------------------- */

let markets = [];          // from markets.json, plus live fields
let bySym = new Map();
let sel = null;
let side = "buy";
let ethUsd = 0;
let sort = { k: "volume_24h_usd", dir: -1 };
let filter = { q: "", held: false, usdg: false, eth: false };

const blank = () => ({ cash: START_CASH, pos: {}, trades: [], realised: 0 });
let acct = load();

function load() {
  try {
    const raw = localStorage.getItem(KEY);
    if (!raw) return blank();
    const a = JSON.parse(raw);
    if (typeof a?.cash !== "number" || !a.pos) return blank();
    return { cash: a.cash, pos: a.pos, trades: a.trades || [], realised: a.realised || 0 };
  } catch { return blank(); }          // private window, cleared storage, blocked
}
function save() {
  try { localStorage.setItem(KEY, JSON.stringify(acct)); } catch { /* not fatal */ }
}

/* ---- chain -------------------------------------------------------------- */

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

/* Send in batches the endpoint will actually accept, and fail loudly if a whole
   batch does. Returning nulls for a failed chunk would leave the page showing
   its last prices while reporting itself live, which is the one failure mode a
   price display must not have. */
async function rpcChunked(calls) {
  const out = [];
  for (let i = 0; i < calls.length; i += BATCH) {
    if (i > 0) await sleep(BATCH_GAP_MS);
    const want = Math.min(BATCH, calls.length - i);
    let part = null;
    /* The endpoint rate-limits in bursts, so one 429 is not an outage. Retry
       the chunk once, further apart, before calling the tick a failure. */
    for (let attempt = 0; attempt < 2 && part === null; attempt++) {
      if (attempt) await sleep(600);
      try {
        const r = await rpcBatch(RPC, calls.slice(i, i + want));
        if (r.length === want) part = r;
      } catch { /* fall through to the retry, then to the throw below */ }
    }
    if (part === null) throw new Error("batch at " + i + " failed twice");
    out.push(...part);
  }
  return out;
}

async function tick() {
  const calls = [{ to: ETH_POOL.addr, data: SEL.slot0 }];
  for (const m of markets) calls.push({ to: m.pool, data: SEL.slot0 });
  if (sel) calls.push({ to: sel.pool, data: SEL.liquidity });

  const res = await rpcChunked(calls);

  const e = res[0];
  if (e) {
    const p = poolPrice(word(e, 0), ETH_POOL.dec0, ETH_POOL.dec1, ETH_POOL.quoteIsToken0);
    if (p > 0) ethUsd = p;
  }
  let seen = 0;
  markets.forEach((m, i) => {
    const r = res[i + 1];
    if (!r) return;
    const sqrtP = word(r, 0);
    const inQuote = poolPrice(sqrtP, m.dec0, m.dec1, m.quote_is_token0);
    if (!(inQuote > 0)) return;
    const usd = m.quote === "eth" ? inQuote * ethUsd : inQuote;
    if (!(usd > 0)) return;
    m.sqrtP = sqrtP;
    m.px = usd;
    if (m.px0 === undefined) m.px0 = usd;
    seen++;
  });

  if (sel) {
    const liq = res[res.length - 1];
    if (liq && sel.sqrtP) {
      const qDec = sel.quote === "eth" ? 18 : 6;
      const d = poolDepth(word(liq, 0), sel.sqrtP, qDec, HAIRCUT_BPS, sel.quote_is_token0);
      const usd = sel.quote === "eth" ? d * ethUsd : d;
      if (usd > 0) sel.liveDepth = usd;
    }
  }
  return seen;
}

/* ---- fill model --------------------------------------------------------- */

/* Closing (or opening) a position of N against a book of B consumes about N/B
 * of the 2% band, so the average fill sits half that away from mid. This is the
 * same linear approximation health.Derive uses to size a position against a
 * book -- deliberately crude, and honest about it: it is a reasonable model
 * while a trade is a small part of the book and nonsense once it is not, which
 * is exactly why the ticket refuses trades past the depth it is modelling. */
function quote(m, usdNotional) {
  const depth = m.liveDepth || m.depth_2pct_usd || 0;
  if (!(m.px > 0) || !(depth > 0) || !(usdNotional > 0)) return null;
  const frac = usdNotional / depth;
  const slipBps = Math.min(frac * 200 / 2, 5000);      // half the band consumed
  const avg = side === "buy" ? m.px * (1 + slipBps / 10000)
                             : m.px * (1 - slipBps / 10000);
  return { slipBps, avg, depth, frac };
}

/* ---- account maths ------------------------------------------------------ */

const posOf = (m) => acct.pos[m.symbol + "/" + m.quote];
function positionsValue() {
  let v = 0;
  for (const m of markets) {
    const p = posOf(m);
    if (p && m.px > 0) v += p.qty * m.px;
  }
  return v;
}
function unrealised() {
  let v = 0;
  for (const m of markets) {
    const p = posOf(m);
    if (p && m.px > 0) v += p.qty * m.px - p.cost;
  }
  return v;
}

function execute() {
  if (!sel) return;
  const raw = parseFloat($("amt").value);
  if (!(raw > 0)) return;
  const key = sel.symbol + "/" + sel.quote;
  const held = acct.pos[key];

  // Buy is entered in USD, sell in units of the asset.
  const notional = side === "buy" ? raw : raw * (sel.px || 0);
  const q = quote(sel, notional);
  if (!q) return;

  if (side === "buy") {
    if (notional > acct.cash + 1e-9) return;
    const qty = notional / q.avg;
    const p = held || (acct.pos[key] = { qty: 0, cost: 0 });
    p.qty += qty;
    p.cost += notional;
    acct.cash -= notional;
    log(sel, "buy", qty, q.avg, notional);
  } else {
    if (!held || raw > held.qty + 1e-12) return;
    const proceeds = raw * q.avg;
    const costOut = held.cost * (raw / held.qty);
    held.qty -= raw;
    held.cost -= costOut;
    acct.cash += proceeds;
    acct.realised += proceeds - costOut;
    if (held.qty <= 1e-12) delete acct.pos[key];
    log(sel, "sell", raw, q.avg, proceeds);
  }
  $("amt").value = "";
  save();
  render();
}

function log(m, s, qty, px, value) {
  acct.trades.unshift({ t: Date.now(), sym: m.symbol, q: m.quote, s, qty, px, value });
  acct.trades = acct.trades.slice(0, 200);
}

/* ---- rendering ---------------------------------------------------------- */

function visible() {
  let xs = markets.filter((m) => {
    if (filter.q && !m.symbol.toLowerCase().includes(filter.q)) return false;
    if (filter.held && !posOf(m)) return false;
    if (filter.usdg && m.quote !== "usdg") return false;
    if (filter.eth && m.quote !== "eth") return false;
    return true;
  });
  const k = sort.k;
  xs.sort((a, b) => {
    let x, y;
    if (k === "symbol") return sort.dir * a.symbol.localeCompare(b.symbol);
    if (k === "chg") { x = chg(a); y = chg(b); }
    else if (k === "posv") { x = (posOf(a)?.qty || 0) * (a.px || 0); y = (posOf(b)?.qty || 0) * (b.px || 0); }
    else if (k === "px") { x = a.px || 0; y = b.px || 0; }
    else { x = a[k] || 0; y = b[k] || 0; }
    return sort.dir * (x - y);
  });
  return xs;
}
const chg = (m) => (m.px > 0 && m.px0 > 0) ? (m.px / m.px0 - 1) * 100 : 0;

function render() {
  const rows = $("rows");
  const xs = visible();
  rows.replaceChildren(...xs.map((m) => {
    const tr = document.createElement("tr");
    if (sel && sel.symbol === m.symbol && sel.quote === m.quote) tr.className = "sel";
    const p = posOf(m);
    const c = chg(m);
    tr.innerHTML =
      `<td><span class="sym">${m.symbol}</span><span class="q">${m.quote}</span></td>` +
      `<td>${m.px ? fmtPx(m.px) : "<span class='mut'>—</span>"}</td>` +
      `<td class="${c > 0 ? "up" : c < 0 ? "down" : "mut"}">${m.px ? (c >= 0 ? "+" : "") + c.toFixed(2) + "%" : "—"}</td>` +
      `<td class="mut">${fmtCompact(m.volume_24h_usd)}</td>` +
      `<td class="mut">${fmtCompact(m.depth_2pct_usd)}</td>` +
      `<td class="mut">${m.score.toLocaleString()}</td>` +
      `<td>${p ? `<span class="held">${fmtQty(p.qty)}</span>` : "<span class='mut'>—</span>"}</td>`;
    tr.onclick = () => { sel = m; render(); };
    return tr;
  }));
  $("mktNote").textContent = xs.length === markets.length
    ? `${markets.length} clearing the gates`
    : `${xs.length} of ${markets.length}`;

  // account strip
  const pv = positionsValue(), u = unrealised();
  $("cash").textContent = fmtUsd(acct.cash);
  $("posval").textContent = fmtUsd(pv);
  $("equity").textContent = fmtUsd(acct.cash + pv);
  $("upnl").textContent = (u >= 0 ? "+" : "") + fmtUsd(u);
  $("upnl").className = "v num " + (u > 0 ? "up" : u < 0 ? "down" : "");
  $("rpnl").textContent = (acct.realised >= 0 ? "+" : "") + fmtUsd(acct.realised);
  $("rpnl").className = "v num " + (acct.realised > 0 ? "up" : acct.realised < 0 ? "down" : "");

  renderTicket();
  renderPositions();
  renderTrades();
}

function renderTicket() {
  const go = $("go");
  if (!sel) {
    $("tSym").textContent = "";
    go.disabled = true; go.textContent = "Select a market";
    return;
  }
  const p = posOf(sel);
  const c = chg(sel);
  $("tSym").textContent = sel.symbol + " / " + sel.quote.toUpperCase();
  $("tPx").textContent = sel.px ? fmtPx(sel.px) : "—";
  $("tChg").textContent = sel.px ? (c >= 0 ? "+" : "") + c.toFixed(2) + "%" : "";
  $("tChg").className = "num " + (c > 0 ? "up" : c < 0 ? "down" : "mut");
  $("tDepth").textContent = fmtCompact(sel.liveDepth || sel.depth_2pct_usd);
  $("tVol").textContent = fmtCompact(sel.volume_24h_usd);
  $("tScore").textContent = sel.score.toLocaleString();
  $("tPos").textContent = p ? fmtQty(p.qty) : "—";
  $("tagLev").textContent = sel.spot_only ? "spot · 1×" : "up to " + sel.max_leverage + "×";
  $("amtUnit").textContent = side === "buy" ? "USD" : sel.symbol;

  const raw = parseFloat($("amt").value);
  const notional = side === "buy" ? raw : raw * (sel.px || 0);
  const q = Number.isFinite(notional) && notional > 0 ? quote(sel, notional) : null;

  $("fMid").textContent = sel.px ? fmtPx(sel.px) : "—";
  $("fSlip").textContent = q ? (q.slipBps / 100).toFixed(2) + "%" : "—";
  $("fAvg").textContent = q ? fmtPx(q.avg) : "—";
  $("fGet").textContent = q
    ? (side === "buy" ? fmtQty(notional / q.avg) + " " + sel.symbol : fmtUsd(raw * q.avg))
    : "—";

  // Why an order might not be allowed. Said plainly, with the fix in it.
  let bad = null;
  if (!(sel.px > 0)) bad = "waiting for a price from chain";
  else if (!Number.isFinite(raw) || raw <= 0) bad = null;
  else if (side === "buy" && notional > acct.cash) bad = "not enough cash";
  else if (side === "sell" && (!p || raw > p.qty)) bad = "you do not hold that much";
  else if (q && q.frac > 0.25) bad = "larger than a quarter of the book";

  go.className = "submit" + (side === "sell" ? " sell" : "");
  go.disabled = bad !== null || !Number.isFinite(raw) || raw <= 0;
  go.textContent = bad ? bad[0].toUpperCase() + bad.slice(1)
    : (side === "buy" ? "Buy " + sel.symbol : "Sell " + sel.symbol);

  const w = $("warn");
  if (q && q.frac > 0.25) {
    w.innerHTML = `<div class="warnbox"><b>Too large for this book.</b> ` +
      `${fmtCompact(notional)} against ${fmtCompact(q.depth)} of executable depth. ` +
      `Past a quarter of the book the linear fill model stops meaning anything, ` +
      `so the ticket refuses rather than quoting a number it cannot stand behind.</div>`;
  } else if (q && q.slipBps > 100) {
    w.innerHTML = `<div class="warnbox">This size moves the pool about ` +
      `<b>${(q.slipBps / 100).toFixed(2)}%</b>. That cost is real and it is why ` +
      `depth, not appetite, sets what a market can carry.</div>`;
  } else w.innerHTML = "";
}

function renderPositions() {
  const held = markets.filter(posOf);
  $("posNote").textContent = held.length ? held.length + " open" : "none";
  $("posRows").replaceChildren(...held.map((m) => {
    const p = posOf(m), val = p.qty * (m.px || 0), pl = val - p.cost;
    const tr = document.createElement("tr");
    tr.innerHTML =
      `<td><span class="sym">${m.symbol}</span><span class="q">${m.quote}</span></td>` +
      `<td>${fmtQty(p.qty)}</td><td>${fmtPx(p.cost / p.qty)}</td>` +
      `<td>${m.px ? fmtPx(m.px) : "—"}</td><td>${fmtUsd(val)}</td>` +
      `<td class="${pl > 0 ? "up" : pl < 0 ? "down" : "mut"}">${(pl >= 0 ? "+" : "") + fmtUsd(pl)}</td>`;
    tr.onclick = () => { sel = m; render(); };
    return tr;
  }));
  if (!held.length) {
    const tr = document.createElement("tr");
    tr.innerHTML = `<td colspan="6" class="empty">No positions. Pick a market and buy.</td>`;
    $("posRows").replaceChildren(tr);
  }
}

function renderTrades() {
  $("tradeNote").textContent = acct.trades.length ? acct.trades.length + " total" : "none yet";
  $("tradeRows").replaceChildren(...acct.trades.slice(0, 60).map((t) => {
    const tr = document.createElement("tr");
    tr.innerHTML =
      `<td class="mut">${new Date(t.t).toLocaleTimeString("en-GB")}</td>` +
      `<td><span class="sym">${t.sym}</span><span class="q">${t.q}</span></td>` +
      `<td class="${t.s === "buy" ? "up" : "down"}">${t.s}</td>` +
      `<td>${fmtQty(t.qty)}</td><td>${fmtPx(t.px)}</td><td>${fmtUsd(t.value)}</td>`;
    return tr;
  }));
  if (!acct.trades.length) {
    const tr = document.createElement("tr");
    tr.innerHTML = `<td colspan="6" class="empty">No fills yet.</td>`;
    $("tradeRows").replaceChildren(tr);
  }
}

/* ---- wiring ------------------------------------------------------------- */

function setConn(ok, text) {
  $("dot").className = "dot " + (ok ? "ok" : "bad");
  $("connText").textContent = text;
}

document.querySelectorAll(".side button").forEach((b) => {
  b.onclick = () => {
    side = b.dataset.side;
    document.querySelectorAll(".side button").forEach((x) => x.classList.toggle("on", x === b));
    $("amt").value = "";
    renderTicket();
  };
});
document.querySelectorAll(".pcts button").forEach((b) => {
  b.onclick = () => {
    if (!sel) return;
    const pct = Number(b.dataset.pct) / 100;
    if (side === "buy") $("amt").value = (acct.cash * pct).toFixed(2);
    else {
      const p = posOf(sel);
      $("amt").value = p ? String(p.qty * pct) : "";
    }
    renderTicket();
  };
});
$("amt").addEventListener("input", renderTicket);
$("go").onclick = execute;
$("q").addEventListener("input", (e) => { filter.q = e.target.value.trim().toLowerCase(); render(); });
for (const [id, key] of [["fHeld", "held"], ["fUsdg", "usdg"], ["fEth", "eth"]]) {
  $(id).onclick = () => {
    filter[key] = !filter[key];
    if (key === "usdg" && filter.usdg) filter.eth = false;
    if (key === "eth" && filter.eth) filter.usdg = false;
    $("fUsdg").classList.toggle("on", filter.usdg);
    $("fEth").classList.toggle("on", filter.eth);
    $("fHeld").classList.toggle("on", filter.held);
    render();
  };
}
document.querySelectorAll("thead th[data-k]").forEach((th) => {
  th.onclick = () => {
    const k = th.dataset.k;
    sort = { k, dir: sort.k === k ? -sort.dir : (k === "symbol" ? 1 : -1) };
    render();
  };
});
$("reset").onclick = () => {
  acct = blank();
  save();
  render();
};

/* ---- boot --------------------------------------------------------------- */

(async function main() {
  try {
    const res = await fetch("/markets.json");
    if (!res.ok) throw new Error("markets " + res.status);
    markets = await res.json();
  } catch (e) {
    setConn(false, "could not load markets");
    return;
  }
  bySym = new Map(markets.map((m) => [m.symbol + "/" + m.quote, m]));
  sel = markets.find((m) => m.symbol === "WETH") || markets[0];
  render();

  let fails = 0;
  async function loop() {
    try {
      const seen = await tick();
      fails = 0;
      setConn(true, seen + " of " + markets.length + " live");
    } catch (e) {
      fails++;
      /* Say it plainly once it is really gone. A price display that goes on
         showing its last number while disconnected is the worst failure mode
         this page has -- but a single rate-limited burst is not that, and
         flashing red on every blip teaches people to ignore the light. */
      if (fails >= 2) setConn(false, "disconnected — prices are stale");
      else setConn(true, "retrying…");
    }
    render();
    setTimeout(loop, POLL_MS);
  }
  loop();
})();
