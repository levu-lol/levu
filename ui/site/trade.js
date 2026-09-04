/* The exchange, from the browser.
 *
 * Everything that decides anything happens on the server: orders are matched by
 * the VM, positions live in a lane, and the leverage a size is offered comes
 * from the risk engine reading that lane's own book. This file asks and draws.
 *
 * It used to simulate all of that locally, which was the right shape when there
 * was no server and the wrong one the moment there was: a fill invented in a
 * tab tests nothing about the engine, and the whole point of a paper round is
 * to find out what the engine does when real people push on it.
 */

import { Chart } from "./chart.js";

const SERVER_URL = "https://92-5-12-15.sslip.io";
const POLL_MS = 4000;

const $ = (id) => document.getElementById(id);
const fmtUsd = (v, dp = 2) =>
  !isFinite(v) ? "—" : (v < 0 ? "-" : "") + "$" + Math.abs(v).toLocaleString("en-US",
    { minimumFractionDigits: dp, maximumFractionDigits: dp });
const fmtCompact = (v) => !isFinite(v) || v <= 0 ? "—"
  : v >= 1e9 ? "$" + (v / 1e9).toFixed(2) + "bn"
  : v >= 1e6 ? "$" + (v / 1e6).toFixed(2) + "m"
  : v >= 1e3 ? "$" + (v / 1e3).toFixed(1) + "k" : "$" + v.toFixed(0);
const fmtPx = (v) => !(v > 0) ? "—"
  : v >= 1000 ? v.toLocaleString("en-US", { maximumFractionDigits: 2 })
  : v >= 1 ? v.toFixed(4) : v >= 1e-4 ? v.toFixed(6) : v.toExponential(3);
const fmtQty = (v) => Math.abs(v) >= 1000 ? v.toLocaleString("en-US", { maximumFractionDigits: 2 })
  : Math.abs(v) >= 1 ? v.toFixed(4) : v.toFixed(8);

/* ---- state -------------------------------------------------------------- */

let reference = [];        // markets.json: what the census measured
let markets = [];          // merged with the server's live view
let sel = null;
let side = "buy";
let account = null;        // { balance, positions } from the server
let pnl = null;            // { granted, net_worth, realised, unrealised }
let fills = [];            // read back from the server, so a refresh keeps them
let openOrders = [];       // resting orders, read from the book each poll
let sort = { k: "volume_24h_usd", dir: -1 };
let wallet = null;         // { address, token }, once signed in
let orderType = "market";  // "market" crosses; "limit" rests
let filter = { q: "", held: false, usdg: false, eth: false };

const keyOf = (m) => m.symbol + "/" + m.quote;

/* ---- the server --------------------------------------------------------- */

async function server(path, opts = {}) {
  const ctl = new AbortController();
  const timer = setTimeout(() => ctl.abort(), 12000);
  try {
    const res = await fetch(SERVER_URL + path, {
      ...opts, signal: ctl.signal,
      headers: {
        "content-type": "application/json",
        ...(wallet ? { "X-Paper-Token": wallet.token } : {}),
        ...(opts.headers || {}),
      },
    });
    const body = await res.json().catch(() => ({}));
    if (!res.ok) throw new Error(body.error || `server ${res.status}`);
    return body;
  } finally {
    clearTimeout(timer);
  }
}

/* One request a tick, and it carries everything: prices, the ladder each market
 * sits on, and — when a session is presented — that account's positions and
 * balance. The browser used to read all 57 pools from chain itself, which was
 * three requests a tick from every viewer for data the server already had. */
async function tick() {
  const st = await server("/api/state" +
    (wallet ? "?token=" + encodeURIComponent(wallet.token) : ""));

  const live = new Map(st.markets.map((m) => [m.pair, m]));
  markets = reference.map((r) => {
    const l = live.get(keyOf(r)) || {};
    return {
      ...r,
      px: l.index || 0,
      confidence: l.confidence || 0,
      leverage: (l.ladder && l.ladder.offered_leverage) || 0,
      earned: (l.ladder && l.ladder.earned_leverage) || 0,
      state: (l.ladder && l.ladder.state) || "—",
      book: (l.ladder && l.ladder.book_depth) || 0,
      needs: (l.ladder && l.ladder.needs_depth) || 0,
      blockers: (l.ladder && l.ladder.blockers) || [],
      score: (l.ladder && l.ladder.score) || r.score,
      maintenance: l.maintenance_bps || 0,
      open: !!l.index,
    };
  }).filter((m) => m.open);

  account = wallet ? { balance: st.balance || {}, positions: st.positions || {} } : null;
  pnl = wallet ? st.pnl || null : null;
  if (wallet) {
    // The round, from the server. Keeping it in a page-local array meant a
    // refresh -- or a second tab -- showed a trader nothing they had done.
    try {
      fills = (await server("/api/fills?limit=100")).fills || [];
    } catch {
      fills = [];
    }
    // From the book, every poll. A resting order fills when somebody else
    // trades, so a list kept here would show orders that are already gone.
    try {
      openOrders = (await server("/api/orders")).orders || [];
    } catch {
      openOrders = [];
    }
  } else {
    openOrders = [];
  }
  if (sel) sel = markets.find((m) => keyOf(m) === keyOf(sel)) || markets[0];
  return markets.length;
}

const posOf = (m) => (account && account.positions && account.positions[keyOf(m)]) || null;
const freeBalance = () => {
  const b = account && account.balance && account.balance.tusdg;
  return b ? Number(b.free) : 0;
};
const allocated = () => {
  const b = account && account.balance && account.balance.tusdg;
  if (!b || !b.allocated) return 0;
  return Object.values(b.allocated).reduce((t, v) => t + Number(v), 0);
};

/* ---- the ticket --------------------------------------------------------- */

/* The quote comes from the exchange, not from a model here.
 *
 * This is the asked-versus-available distinction made visible: you name a size,
 * and the engine answers with the leverage that size can actually have, which
 * falls as the size grows against the book behind it. */
let quoteSeq = 0;
async function refreshQuote() {
  const el = { n: $("fNotional"), l: $("fLev"), m: $("fMargin"), f: $("fFree") };
  el.f.textContent = wallet ? fmtUsd(freeBalance()) : "connect a wallet";
  const qty = parseFloat($("amt").value);
  if (!sel || !(qty > 0)) {
    el.n.textContent = el.l.textContent = el.m.textContent = "—";
    return paintSubmit(null);
  }
  const mine = ++quoteSeq;
  try {
    const limit = orderType === "limit" ? parseFloat($("limitPx").value) : 0;
    const q = await server(`/api/quote?pair=${encodeURIComponent(keyOf(sel))}&qty=${qty}` +
      `&side=${side}` + (limit > 0 ? `&price=${limit}` : ""));
    if (mine !== quoteSeq) return;      // a later keystroke already answered
    el.n.textContent = fmtUsd(q.notional);
    el.l.textContent = q.your_leverage > 0
      ? q.your_leverage + "×" + (q.your_leverage < q.headline_leverage
          ? ` (market offers ${q.headline_leverage}×)` : "")
      : "not offerable";
    el.m.textContent = q.margin_required ? fmtUsd(q.margin_required) : "—";
    paintSubmit(q);
  } catch (err) {
    if (mine !== quoteSeq) return;
    el.n.textContent = el.l.textContent = el.m.textContent = "—";
    paintSubmit(null, err.message);
  }
}

function paintSubmit(q, err) {
  const go = $("go"), warn = $("warn");
  const qty = parseFloat($("amt").value);
  let stop = null;
  if (!sel) stop = "Select a market";
  else if (!wallet) stop = "Connect a wallet to trade";
  else if (!(qty > 0)) stop = (side === "buy" ? "Long " : "Short ") + (sel ? sel.symbol : "");
  else if (err) stop = err;
  else if (!q) stop = "Pricing…";
  else if (!(q.your_leverage > 0)) stop = q.note || "Not offerable at this size";
  else if (q.margin_required > freeBalance()) stop = "Not enough free tUSDG";

  const ready = q && q.your_leverage > 0 && qty > 0 && wallet &&
                q.margin_required <= freeBalance();
  go.className = "submit" + (side === "sell" ? " sell" : "");
  go.disabled = !ready;
  go.textContent = ready
    ? (side === "buy" ? "Long " : "Short ") + sel.symbol
    : stop;

  warn.innerHTML = "";
  if (q && q.note) {
    warn.innerHTML = `<div class="warnbox"><b>${q.note}.</b> The leverage a size is
      offered comes from the book that would have to close it, so it falls as the
      position grows. This is the engine answering, not a cap someone typed.</div>`;
  } else if (sel && sel.leverage <= 1 && sel.blockers && sel.blockers.length) {
    warn.innerHTML = `<div class="warnbox">This market is on the <b>spot rung</b>:
      ${sel.blockers[0]}. It trades at 1×, fully collateralised, until that changes.</div>`;
  } else if (sel && sel.leverage <= 1 && sel.needs > sel.book) {
    warn.innerHTML = `<div class="warnbox">Spot only until the book deepens —
      <b>${fmtCompact(sel.book)}</b> of the ${fmtCompact(sel.needs)} it needs to be levered
      against. Resting orders here are what move it.</div>`;
  }
}

async function submit() {
  const qty = parseFloat($("amt").value);
  if (!sel || !(qty > 0) || !wallet) return;
  const go = $("go");
  go.disabled = true;
  go.textContent = "Sending…";
  try {
    const limit = orderType === "limit" ? parseFloat($("limitPx").value) : 0;
    if (orderType === "limit" && !(limit > 0)) throw new Error("Enter a limit price");

    const out = await server("/api/order", {
      method: "POST",
      body: JSON.stringify({
        token: wallet.token, pair: keyOf(sel), side, qty,
        market: orderType === "market",
        ...(orderType === "limit" ? { price: limit } : {}),
      }),
    });

    // "accepted" means the lane took the submission, not that anything
    // happened. The receipts say what the VM did, and an order rejected for
    // margin arrives inside a 200 -- which is how a rejection came to be drawn
    // as a fill that then vanished on the next poll.
    const receipts = out.receipts || [];
    const bad = receipts.find((r) => /^rejected/i.test(r));
    if (bad) throw new Error(bad.replace(/^rejected:\s*/i, ""));

    const filled = receipts.find((r) => /^filled/i.test(r));
    const resting = receipts.find((r) => /^resting/i.test(r));
    if (filled) {
      const at = parseFloat((filled.match(/@\s*([\d.]+)/) || [])[1]) || sel.px;
      fills.unshift({ t: Date.now(), pair: keyOf(sel), side, qty, px: at, seq: out.seq });
      fills = fills.slice(0, 60);
      note(`Filled ${qty} ${sel.symbol} at ${fmtPx(at)}.`);
    } else if (resting) {
      note(`Resting on the book at ${fmtPx(limit)} — it fills when the market reaches it.`);
    } else {
      note(receipts.join("; ") || "The lane accepted the order.");
    }
    $("amt").value = "";
    await refresh();
  } catch (err) {
    note(err.message);
  } finally {
    // The button belongs to the quote, and only the quote repaints it. Without
    // this it sat on "Sending…" after a perfectly good fill until the next
    // keystroke -- which read as an order that never went anywhere.
    await refreshQuote();
  }
}

/* Closing goes through the exchange like anything else: the lane reduces the
 * position against its own book, and refuses if it cannot. There is no local
 * shortcut that marks a position closed without the VM agreeing. */
async function closePosition(m, btn, fraction = 1) {
  if (!wallet) return;
  const p = posOf(m);
  if (!p) return;
  btn.disabled = true;
  btn.textContent = fraction < 1 ? "Closing…" : "Closing…";
  try {
    // The server clamps qty to the position, so a fill landing between this
    // read and the request cannot turn a close into an order the other way.
    const body = { token: wallet.token, pair: keyOf(m) };
    if (fraction < 1) body.qty = Math.abs(p.size) * fraction;
    const out = await server("/api/close", {
      method: "POST",
      body: JSON.stringify(body),
    });
    const bad = (out.receipts || []).find((r) => /^rejected/i.test(r));
    if (bad) throw new Error(bad.replace(/^rejected:\s*/i, ""));
    note(fraction < 1 ? `Closed ${Math.round(fraction * 100)}% of ${m.symbol}.`
                      : `Closed ${m.symbol}.`);
    await refresh();
  } catch (err) {
    note("Could not close: " + err.message);
    btn.disabled = false;
    btn.textContent = btn.dataset.label || "Close";
  }
}

/* ---- rendering ---------------------------------------------------------- */

function visible() {
  const xs = markets.filter((m) => {
    if (filter.q && !m.symbol.toLowerCase().includes(filter.q)) return false;
    if (filter.held && !posOf(m)) return false;
    if (filter.usdg && m.quote !== "usdg") return false;
    if (filter.eth && m.quote !== "eth") return false;
    return true;
  });
  const k = sort.k;
  xs.sort((a, b) => {
    if (k === "symbol") return sort.dir * a.symbol.localeCompare(b.symbol);
    // Stage is a word, and subtracting words gives NaN, which sorts nothing.
    if (k === "state") return sort.dir * String(a.state).localeCompare(String(b.state));
    const get = (m) => k === "px" ? m.px
      : k === "lev" ? m.leverage
      : k === "posv" ? Math.abs((posOf(m) || {}).size || 0) * m.px
      : m[k] || 0;
    return sort.dir * (get(a) - get(b));
  });
  return xs;
}

function render() {
  const xs = visible();
  $("rows").replaceChildren(...xs.map((m) => {
    const tr = document.createElement("tr");
    if (sel && keyOf(sel) === keyOf(m)) tr.className = "sel";
    const p = posOf(m);
    tr.innerHTML =
      `<td><span class="sym">${m.symbol}</span><span class="q">${m.quote}</span></td>` +
      `<td>${fmtPx(m.px)}</td>` +
      `<td class="${m.state === "live" ? "up" : "mut"}">${m.state}</td>` +
      `<td class="mut">${fmtCompact(m.volume_24h_usd)}</td>` +
      `<td class="mut">${fmtCompact(m.depth_2pct_usd)}</td>` +
      `<td class="mut">${fmtCompact(m.book)}</td>` +
      `<td class="${m.leverage > 1 ? "up" : "mut"}">${m.leverage > 0 ? m.leverage + "×" : "—"}</td>` +
      `<td class="mut">${(m.score || 0).toLocaleString()}</td>` +
      `<td>${p ? `<span class="held">${fmtQty(p.size)}</span>` : "<span class='mut'>—</span>"}</td>`;
    tr.onclick = () => { sel = m; render(); refreshQuote(); };
    return tr;
  }));
  $("mktNote").textContent = xs.length === markets.length
    ? `${markets.length} live` : `${xs.length} of ${markets.length}`;

  const positions = markets.filter(posOf);
  let unreal = 0;
  for (const m of positions) unreal += Number(posOf(m).unrealised_pnl || 0);

  // The server's own accounting where it has it: net worth minus what the
  // faucet granted, split by the marks on what is still open. Adding the page's
  // three numbers together was a second accounting of the same money.
  const worth = pnl ? pnl.net_worth : freeBalance() + allocated() + unreal;
  const un = pnl ? pnl.unrealised : unreal;
  const re = pnl ? pnl.realised : 0;

  $("cash").textContent = wallet ? fmtUsd(freeBalance()) : "—";
  $("posval").textContent = wallet ? fmtUsd(allocated()) : "—";
  $("equity").textContent = wallet ? fmtUsd(worth) : "—";
  signed($("upnl"), wallet ? un : null);
  signed($("realised"), wallet ? re : null);
  $("rpnl").textContent = String(positions.length);

  renderTicket();
  loadChart(sel);
  loadBook(sel);
  pushChartPoint(sel);
  renderPositions(positions);
  renderOpenOrders();
  renderFills();
}

// signed paints a gain or a loss, or an em dash when there is no account.
function signed(el, v) {
  if (v === null || v === undefined) {
    el.textContent = "—";
    el.className = "v num";
    return;
  }
  el.textContent = (v >= 0 ? "+" : "") + fmtUsd(v);
  el.className = "v num " + (v > 0.005 ? "up" : v < -0.005 ? "down" : "");
}

function renderTicket() {
  if (!sel) { $("tSym").textContent = ""; return; }
  const p = posOf(sel);
  $("tSym").textContent = sel.symbol + " / " + sel.quote.toUpperCase();
  $("tPx").textContent = fmtPx(sel.px);
  $("tChg").textContent = sel.state;
  $("tChg").className = "num " + (sel.state === "live" ? "up" : "mut");
  // Two different numbers, and the difference is the whole engine: the venue's
  // depth bounds what it costs to move the price, our book bounds what a
  // forced close can eat. This column showed the second under the first's
  // label, which is exactly the conflation the risk engine spent so long
  // separating.
  $("tDepth").textContent = fmtCompact(sel.depth_2pct_usd);
  $("tBook").textContent = fmtCompact(sel.book);
  $("tVol").textContent = fmtCompact(sel.volume_24h_usd);
  $("tScore").textContent = (sel.score || 0).toLocaleString();
  $("tPos").textContent = p ? fmtQty(p.size) : "—";
  $("tagLev").textContent = sel.leverage > 1 ? "up to " + sel.leverage + "×" : "spot · 1×";
  $("amtUnit").textContent = sel.symbol;
}

function renderPositions(positions) {
  $("posNote").textContent = positions.length ? positions.length + " open" : "none";
  $("posRows").replaceChildren(...positions.map((m) => {
    const p = posOf(m);
    const pl = Number(p.unrealised_pnl || 0);
    const tr = document.createElement("tr");
    tr.innerHTML =
      `<td><span class="sym">${m.symbol}</span><span class="q">${m.quote}</span></td>` +
      `<td class="${p.size > 0 ? "up" : "down"}">${fmtQty(p.size)}</td>` +
      `<td>${fmtPx(p.entry_price)}</td><td>${fmtPx(m.px)}</td>` +
      `<td class="${p.liquidation > 0 ? "down" : "mut"}">${p.liquidation > 0 ? fmtPx(p.liquidation) : "—"}</td>` +
      `<td class="${pl > 0 ? "up" : pl < 0 ? "down" : "mut"}">${(pl >= 0 ? "+" : "") + fmtUsd(pl)}</td>` +
      `<td class="closers">` +
        `<button class="close half" data-label="Half">Half</button>` +
        `<button class="close" data-label="Close">Close</button></td>`;
    tr.onclick = (e) => {
      if (e.target.classList.contains("half")) return closePosition(m, e.target, 0.5);
      if (e.target.classList.contains("close")) return closePosition(m, e.target);
      sel = m; render(); refreshQuote();
    };
    return tr;
  }));
  if (!positions.length) {
    const tr = document.createElement("tr");
    tr.innerHTML = `<td colspan="6" class="empty">No positions. Pick a market and open one.</td>`;
    $("posRows").replaceChildren(tr);
  }
}

function renderOpenOrders() {
  $("ordNote").textContent = openOrders.length ? openOrders.length + " resting" : "none";
  $("ordRows").replaceChildren(...openOrders.map((o) => {
    const [sym, q] = o.pair.split("/");
    const tr = document.createElement("tr");
    tr.innerHTML =
      `<td><span class="sym">${sym}</span><span class="q">${q}</span></td>` +
      `<td class="${o.side === "buy" ? "up" : "down"}">${o.side === "buy" ? "long" : "short"}</td>` +
      `<td>${fmtQty(o.qty)}</td><td>${fmtPx(o.price)}</td>` +
      `<td><button class="close" data-id="${o.id}" data-pair="${o.pair}">Cancel</button></td>`;
    tr.querySelector("button").onclick = (e) => cancelOrder(o, e.target);
    return tr;
  }));
  if (!openOrders.length) {
    const tr = document.createElement("tr");
    tr.innerHTML = `<td colspan="5" class="empty">No resting orders.</td>`;
    $("ordRows").replaceChildren(tr);
  }
}

async function cancelOrder(o, btn) {
  if (!wallet) return;
  btn.disabled = true;
  btn.textContent = "…";
  try {
    await server("/api/cancel", {
      method: "POST",
      body: JSON.stringify({ token: wallet.token, pair: o.pair, id: o.id }),
    });
    note(`Cancelled order ${o.id}.`);
    await refresh();
  } catch (err) {
    note("Could not cancel: " + err.message);
    btn.disabled = false;
    btn.textContent = "Cancel";
  }
}

const KIND = {
  fill: ["", "filled"],
  close: ["mut", "closed"],
  liquidation: ["down", "liquidated"],
  refused: ["down", "refused"],
};

function renderFills() {
  $("tradeNote").textContent = fills.length ? fills.length + " so far" : "none yet";
  $("tradeRows").replaceChildren(...fills.map((f) => {
    const tr = document.createElement("tr");
    const [sym, q] = (f.pair || "/").split("/");
    const [cls, label] = KIND[f.kind] || ["mut", f.kind];
    const what = f.kind === "fill"
      ? `<span class="${f.side === "buy" ? "up" : "down"}">${f.side === "buy" ? "long" : "short"}</span>`
      : `<span class="${cls}">${label}</span>`;
    tr.innerHTML =
      `<td class="mut">${new Date(f.at).toLocaleTimeString("en-GB")}</td>` +
      `<td><span class="sym">${sym}</span><span class="q">${q}</span></td>` +
      `<td>${what}</td>` +
      `<td>${f.qty ? fmtQty(f.qty) : "—"}</td>` +
      `<td>${f.price ? fmtPx(f.price) : "—"}</td>` +
      `<td class="mut" title="${(f.reason || "").replace(/"/g, "&quot;")}">${
        f.reason ? f.reason.slice(0, 40) : ""}</td>`;
    return tr;
  }));
  if (!fills.length) {
    const tr = document.createElement("tr");
    tr.innerHTML = `<td colspan="6" class="empty">No orders yet.</td>`;
    $("tradeRows").replaceChildren(tr);
  }
}

/* ---- chart -------------------------------------------------------------- */

/* The same component the terminal uses, on the market being traded.
 *
 * A trader deciding a direction with no price history in front of them is
 * being asked to trade on a single number. paperd already keeps a day of every
 * market on its own tick, so this is a fetch rather than a data problem. */
let chart = null;
let chartPair = null;
const chartTicks = [];

function ensureChart() {
  if (chart) return chart;
  const cv = $("tChart");
  if (!cv) return null;
  chart = new Chart(cv, {
    bandBps: 0,
    fmt: (v) => fmtPx(v),
    fmtDepth: (v) => fmtCompact(v),
  });
  chart.setBand(false);
  chart.setMark(false);
  chart.setDepth(false);
  chart.setTimeframe(60_000);
  return chart;
}

async function loadChart(m) {
  const c = ensureChart();
  if (!c || !m) return;
  const pair = keyOf(m);
  if (pair === chartPair) return;
  chartPair = pair;
  chartTicks.length = 0;
  c.setTicks([]);
  $("chartNote").textContent = "loading…";
  try {
    const out = await server("/api/history?pair=" + encodeURIComponent(pair) + "&minutes=1440");
    if (chartPair !== pair) return;          // the trader moved on while we waited
    for (const [t, price] of out.points || []) {
      if (price > 0) chartTicks.push(tickAt(t, price));
    }
  } catch {
    // A market with no history is a shorter chart, not a broken page.
  }
  if (chartPair !== pair) return;
  $("chartNote").textContent = chartTicks.length ? "" : "no history yet";
  c.setTicks(chartTicks.slice());
}

const tickAt = (t, price) => ({
  at: new Date(t), t, index: price, mark: price,
  agg: { confidence: 0, used: [], rejected: [] },
  sources: [], failed: [], depthTotal: 0, depth: 0, conf: 0,
});

/* One point per poll, so the chart keeps moving while the page is open. */
function pushChartPoint(m) {
  if (!chart || !m || keyOf(m) !== chartPair || !(m.px > 0)) return;
  const t = Date.now();
  const last = chartTicks[chartTicks.length - 1];
  if (last && t - last.t < 1000) return;
  chartTicks.push(tickAt(t, m.px));
  if (chartTicks.length > 4000) chartTicks.shift();
  $("chartNote").textContent = "";
  chart.setTicks(chartTicks.slice());
}

/* ---- the book ------------------------------------------------------------ */

/* The shape of the book, not just its total.
 *
 * This exchange's whole claim is that leverage comes from the depth a position
 * would be closed into. A trader could read that as one number and never see
 * whether it was one order at one price or twenty levels deep -- which is the
 * difference between a book that absorbs a close and one that walks the price
 * doing it. */
let bookPair = null;
let bookAt = 0;
let bookBusy = false;

async function loadBook(m) {
  if (!m) return;
  const pair = keyOf(m);
  // render() runs on every tick and on every click; the book only needs
  // refetching when the market changes or a couple of seconds have passed.
  const stale = pair !== bookPair || Date.now() - bookAt > 2500;
  if (bookBusy || !stale) return;
  bookBusy = true;
  bookAt = Date.now();
  try {
    const b = await server("/api/book?levels=8&pair=" + encodeURIComponent(pair));
    if (keyOf(sel || {}) !== pair) return;     // the trader moved on
    bookPair = pair;
    renderBook(b);
  } catch {
    if (keyOf(sel || {}) === pair) renderBook(null);
  } finally {
    bookBusy = false;
  }
}

function renderBook(b) {
  const asks = (b && b.asks) || [];
  const bids = (b && b.bids) || [];
  const peak = Math.max(
    ...asks.map((l) => l.cumulative), ...bids.map((l) => l.cumulative), 1);

  const row = (l, cls) => {
    const d = document.createElement("div");
    d.className = "lrow " + cls;
    d.innerHTML =
      `<span class="bar" style="width:${Math.max(2, (l.cumulative / peak) * 100)}%"></span>` +
      `<span class="px">${fmtPx(l.price)}</span>` +
      `<span class="qty">${fmtQty(l.qty)}</span>` +
      `<span class="cum">${fmtCompact(l.cumulative)}</span>`;
    return d;
  };

  /* Asks read downwards to the spread, the way every book is drawn. */
  $("askRows").replaceChildren(...asks.slice().reverse().map((l) => row(l, "ask")));
  $("bidRows").replaceChildren(...bids.map((l) => row(l, "bid")));

  if (!asks.length && !bids.length) {
    const e = document.createElement("div");
    e.className = "empty";
    e.textContent = "Nothing resting. A market order here has nothing to fill against.";
    $("askRows").replaceChildren(e);
  }

  const bestAsk = asks.length ? asks[0].price : 0;
  const bestBid = bids.length ? bids[0].price : 0;
  $("ladderMid").textContent = (b && b.index > 0) ? fmtPx(b.index) : "—";
  $("spreadNote").textContent = bestAsk && bestBid
    ? ((bestAsk - bestBid) / ((bestAsk + bestBid) / 2) * 10000).toFixed(0) + " bps wide"
    : "one-sided";
}

function setConn(ok, text) {
  $("dot").className = "dot " + (ok ? "ok" : "bad");
  $("connText").textContent = text;
}

async function refresh() {
  const n = await tick();
  setConn(true, n + " markets live");
  render();
}

/* ---- wiring ------------------------------------------------------------- */

let quoteTimer = null;

function setOrderType(t) {
  orderType = t;
  $("tMarket").classList.toggle("on", t === "market");
  $("tLimit").classList.toggle("on", t === "limit");
  $("limitRow").hidden = t !== "limit";
  if (t === "limit" && sel && !$("limitPx").value) $("limitPx").value = sel.px.toFixed(4);
  refreshQuote();
}
$("tMarket").onclick = () => setOrderType("market");
$("tLimit").onclick = () => setOrderType("limit");
$("limitPx").addEventListener("input", () => {
  clearTimeout(quoteTimer);
  quoteTimer = setTimeout(refreshQuote, 220);
});

document.querySelectorAll(".side button").forEach((b) => {
  b.onclick = () => {
    side = b.dataset.side;
    document.querySelectorAll(".side button").forEach((x) => x.classList.toggle("on", x === b));
    refreshQuote();
  };
});
document.querySelectorAll(".pcts button").forEach((b) => {
  b.onclick = async () => {
    if (!sel || !wallet || !(sel.px > 0)) return;
    // Size from the margin available and the leverage this market offers, then
    // let the quote tell us what that size is actually worth in leverage --
    // which may be less, and that is the point.
    const lev = Math.max(1, sel.leverage);
    const budget = freeBalance() * (Number(b.dataset.pct) / 100);
    $("amt").value = ((budget * lev) / sel.px).toFixed(6);
    refreshQuote();
  };
});

$("amt").addEventListener("input", () => {
  clearTimeout(quoteTimer);
  quoteTimer = setTimeout(refreshQuote, 220);   // one quote per pause, not per keystroke
});
$("go").onclick = submit;
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

/* ---- boot --------------------------------------------------------------- */

/* ---- wallet ------------------------------------------------------------- *
 *
 * The identity a trader on Robinhood Chain already has is their address, so
 * connecting a wallet does not create a second one -- it names the one they
 * have. The server derives the account from the address directly: twenty bytes
 * either way, no mapping to keep, and the same wallet is the same account on
 * another device or after the server is rebuilt.
 *
 * Nothing here ever sees a private key. The wallet signs a message we hand it
 * and the server recovers the signer; we could not spend anything even if we
 * wanted to, and the message says so in words the signer can read.
 */

const RH_CHAIN = { id: 4663, hex: "0x1237" };
const WKEY = "levu.wallet.v1";

function loadWallet() {
  try {
    const raw = localStorage.getItem(WKEY);
    return raw ? JSON.parse(raw) : null;
  } catch { return null; }
}
function saveWallet(w) {
  try {
    if (w) localStorage.setItem(WKEY, JSON.stringify(w));
    else localStorage.removeItem(WKEY);
  } catch { /* private window; the session simply does not persist */ }
}

/* Ask the wallet to move to Robinhood Chain, offering to add it if unknown.
 *
 * Signing does not strictly need the right chain -- the message carries the
 * chain id and the server checks nothing else -- but a wallet pointed at
 * somewhere else is a trader who will be confused later, so it is worth
 * settling now. */
async function ensureChain(eth) {
  const current = await eth.request({ method: "eth_chainId" });
  if (current === RH_CHAIN.hex) return true;
  try {
    await eth.request({
      method: "wallet_switchEthereumChain",
      params: [{ chainId: RH_CHAIN.hex }],
    });
    return true;
  } catch (err) {
    if (err && err.code === 4902) {
      try {
        await eth.request({
          method: "wallet_addEthereumChain",
          params: [{
            chainId: RH_CHAIN.hex,
            chainName: "Robinhood Chain",
            nativeCurrency: { name: "Ether", symbol: "ETH", decimals: 18 },
            rpcUrls: ["https://rpc.mainnet.chain.robinhood.com"],
          }],
        });
        return true;
      } catch { /* declined */ }
    }
    return false; // declining is a choice, not an error
  }
}

async function connectWallet() {
  const eth = window.ethereum;
  if (!eth) {
    note("No wallet found in this browser. Paper trading works without one — " +
         "an account only matters for the tUSDG faucet.");
    return;
  }
  const btn = $("connect");
  btn.disabled = true;
  btn.textContent = "Connecting…";
  try {
    const accounts = await eth.request({ method: "eth_requestAccounts" });
    if (!accounts || !accounts.length) throw new Error("no account offered");
    const address = accounts[0];
    await ensureChain(eth);

    const { message } = await server("/api/nonce", {
      method: "POST", body: JSON.stringify({ address }),
    });
    // personal_sign: the wallet shows the text and signs it. It authorises no
    // transaction and costs no gas, and the message says exactly that.
    const signature = await eth.request({
      method: "personal_sign", params: [message, address],
    });
    const out = await server("/api/connect", {
      method: "POST", body: JSON.stringify({ address, signature }),
    });
    wallet = { address: out.address, token: out.token };
    saveWallet(wallet);
    note(out.can_claim ? "Connected. The faucet has today's grant waiting."
                       : "Connected. Today's grant is already claimed.");
    await refreshWallet();
  } catch (err) {
    // A rejected signature is somebody saying no, not a failure to report as one.
    note(err && err.code === 4001 ? "Signature declined." : "Could not connect: " + err.message);
  } finally {
    btn.disabled = false;
    paintWallet();
  }
}

async function refreshWallet() {
  if (!wallet) { paintWallet(); return; }
  try {
    await refresh();          // one poll carries prices, positions and balance
    $("walletBal").textContent = Number(freeBalance()).toLocaleString("en-US",
      { maximumFractionDigits: 2 });
  } catch { /* the page works without it; the connection light says so */ }
  paintWallet();
  refreshQuote();
}

async function claim() {
  if (!wallet) return;
  const btn = $("claim");
  btn.disabled = true;
  try {
    const out = await server("/api/faucet", { method: "POST" });
    note(`Received ${Number(out.granted).toLocaleString("en-US")} tUSDG. ` +
         `Next grant after ${new Date(out.next_claim).toLocaleString()}.`);
    await refreshWallet();
  } catch (err) {
    note(err.message);
  } finally {
    btn.disabled = false;
  }
}

function note(text) { $("walletNote").textContent = text; }

function paintWallet() {
  const bar = $("walletBar");
  if (!wallet) {
    bar.hidden = true;
    $("connect").textContent = "Connect wallet";
    return;
  }
  bar.hidden = false;
  const a = wallet.address;
  $("walletAddr").textContent = a.slice(0, 6) + "…" + a.slice(-4);
  $("connect").textContent = "Disconnect";
}

$("connect").onclick = () => {
  if (wallet) {
    wallet = null;
    saveWallet(null);
    note("");
    paintWallet();
    return;
  }
  connectWallet();
};
$("claim").onclick = claim;

/* A wallet that changes account in the extension is a different trader. */
if (window.ethereum && window.ethereum.on) {
  window.ethereum.on("accountsChanged", (accs) => {
    if (!wallet) return;
    if (!accs.length || accs[0].toLowerCase() !== wallet.address.toLowerCase()) {
      wallet = null;
      saveWallet(null);
      note("Wallet changed account — reconnect to sign in as that address.");
      paintWallet();
    }
  });
}

(async function main() {
  // Restore a session before the first poll, so the first request already
  // carries it and the account is populated on the first paint.
  wallet = loadWallet();
  paintWallet();
  try {
    const res = await fetch("/markets.json");
    reference = await res.json();
  } catch {
    setConn(false, "could not load the market list");
    return;
  }
  try {
    await refresh();
    sel = markets.find((m) => m.symbol === "WETH") || markets[0];
    render();
    refreshQuote();
  } catch (err) {
    setConn(false, "exchange unreachable");
  }

  let fails = 0;
  (async function loop() {
    try {
      await refresh();
      fails = 0;
    } catch {
      fails++;
      // One missed poll is not an outage, and saying so on the first would
      // teach people to ignore the light.
      if (fails >= 2) setConn(false, "exchange unreachable — figures are stale");
    }
    setTimeout(loop, POLL_MS);
  })();
})();
