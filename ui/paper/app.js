/* The paper terminal.
 *
 * Talks to paperd, which puts these orders through the real PerpVM against
 * prices read live from Robinhood Chain. Nothing here simulates anything: when
 * an order is refused it is the matching engine refusing it, and the reason it
 * gives is the reason the engine gave.
 */

const $ = (id) => document.getElementById(id);
const S = { token: null, addr: null, last: null, ticks: [], max: 400,
            pair: null, markets: [], balance: null, positions: {} };

const money = (v, dp = 2) =>
  !isFinite(v) ? "—" : v.toLocaleString("en-US", { minimumFractionDigits: dp, maximumFractionDigits: dp });

/* ---- session ------------------------------------------------------------ */

/* The token is kept in localStorage so a refresh does not orphan a position.
 * It is not kept anywhere else and it is not recoverable: paperd holds sessions
 * in memory, so a server restart ends them. That is deliberate -- a paper
 * identity should not be something anyone builds on. */
try {
  const saved = JSON.parse(localStorage.getItem("paper") || "null");
  if (saved && saved.token) { S.token = saved.token; S.addr = saved.address; }
} catch { /* a cleared or hostile store is the same as no session */ }

async function api(path, body) {
  const res = await fetch(path, {
    method: body ? "POST" : "GET",
    headers: body ? { "content-type": "application/json" } : undefined,
    body: body ? JSON.stringify(body) : undefined,
  });
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(data.error || `http ${res.status}`);
  return data;
}

async function start() {
  $("start").disabled = true;
  try {
    const s = await api("/api/session", {});
    S.token = s.token; S.addr = s.address;
    localStorage.setItem("paper", JSON.stringify({ token: s.token, address: s.address }));
    log(`stake issued: ${money(s.granted, 0)} paper USDG`, "ok");
    showAccount();
  } catch (e) {
    log(String(e.message || e), "err");
    $("start").disabled = false;
  }
}

function showAccount() {
  $("noacct").hidden = true;
  $("acct").hidden = false;
  $("ticket").hidden = false;
  $("addr").textContent = S.addr ? S.addr.slice(0, 10) + "…" : "—";
}
if (S.token) showAccount();

/* ---- orders -------------------------------------------------------------- */

async function order(side) {
  const qty = parseFloat($("qty").value);
  const raw = $("price").value.trim();
  const price = raw === "" ? 0 : parseFloat(raw);
  if (!(qty > 0)) { log("quantity must be positive", "err"); return; }
  if (raw !== "" && !(price > 0)) { log("price must be positive", "err"); return; }

  setBusy(true);
  try {
    const r = await api("/api/order", {
      token: S.token, pair: S.pair, side, qty, price, market: raw === "",
    });
    for (const line of r.receipts || []) log(`${S.pair}  ${line}`, "ok");
    if (r.balance) { S.balance = r.balance; paintBalance(); }
    if (!r.receipts || r.receipts.length === 0) log("accepted", "ok");
    paintPosition(r.position);
  } catch (e) {
    /* A rejection is the engine working. Margin refused, self-trade prevented,
     * lot size wrong -- all of these are the VM saying no for a reason, and the
     * reason is worth more to the trader than the word "failed". */
    log(String(e.message || e), "err");
  } finally { setBusy(false); }
}

async function flatten() {
  setBusy(true);
  try {
    const r = await api("/api/close", { token: S.token, pair: S.pair });
    log(r.closed ? "position closed" : (r.reason || "nothing to close"), r.closed ? "ok" : "");
    if (r.position) paintPosition(r.position);
    if (r.balance) { S.balance = r.balance; paintBalance(); }
  } catch (e) { log(String(e.message || e), "err"); }
  finally { setBusy(false); }
}

function setBusy(b) { for (const id of ["buy", "sell", "flat"]) $(id).disabled = b; }

/* What this size actually gets, while they are still typing.
 *
 * The market's headline leverage is what a small position receives. Showing
 * that to somebody about to open a large one and then filling them at less
 * reads as a bug even when it is the risk engine working exactly as intended. */
let quoteTimer = null;
async function quote() {
  const qty = parseFloat($("qty").value);
  if (!(qty > 0)) { $("qlev").textContent = "—"; $("qnote").textContent = ""; return; }
  try {
    const q = await api("/api/quote?pair=" + encodeURIComponent(S.pair) +
                        "&qty=" + encodeURIComponent(qty));
    const lev = q.your_leverage;
    $("qlev").textContent = lev > 0 ? lev + "×" : "not offerable";
    $("qlev").style.color =
      lev === 0 ? "var(--down)" : lev < q.headline_leverage ? "var(--ember)" : "";
    $("qnote").textContent = q.note
      ? q.note + (q.margin_required ? ` · needs ${money(q.margin_required)} margin` : "")
      : (q.margin_required ? `needs ${money(q.margin_required)} margin` : "");
  } catch { /* the order path will say so properly if it matters */ }
}
$("qty").addEventListener("input", () => {
  clearTimeout(quoteTimer);
  quoteTimer = setTimeout(quote, 250);
});

$("start").onclick = start;
$("buy").onclick = () => order("buy");
$("sell").onclick = () => order("sell");
$("flat").onclick = flatten;

/* ---- state --------------------------------------------------------------- */

function paintPosition(p) {
  if (!p) return;
  $("eq").textContent = money(p.equity);
  $("free").textContent = money(p.free);
  $("size").textContent = p.size === 0 ? "flat" : money(p.size, 4);
  $("entry").textContent = p.size === 0 ? "—" : money(p.entry_price);
  const pnl = $("pnl");
  pnl.textContent = money(p.unrealised_pnl);
  pnl.style.color = p.unrealised_pnl > 0 ? "var(--up)" : p.unrealised_pnl < 0 ? "var(--down)" : "";
  $("liq").textContent = p.size === 0 || !p.liquidation ? "—" : money(p.liquidation);
}

function paint(st) {
  /* One balance, several markets. The selector picks which lane the ticket
     acts on; the balance panel is the whole account, because that is the view
     shared margin exists to make possible. */
  S.markets = st.markets || [];
  if (!S.pair && S.markets.length) S.pair = S.markets[0].pair;
  const sel = $("pairSel");
  const want = S.markets.map((m) => m.pair).join("|");
  if (sel.dataset.built !== want) {
    sel.innerHTML = S.markets.map((m) =>
      `<option value="${m.pair}"${m.pair === S.pair ? " selected" : ""}>${m.pair}</option>`).join("");
    sel.dataset.built = want;
  }
  if (st.balance) S.balance = st.balance;
  if (st.positions) S.positions = st.positions;
  paintBalance();

  const m = S.markets.find((x) => x.pair === S.pair);
  if (!m) return;
  if (S.positions[S.pair]) paintPosition(S.positions[S.pair]);
  else paintPosition({ size: 0, equity: 0, free: 0, unrealised_pnl: 0, entry_price: 0, liquidation: 0 });

  const st2 = m;
  const el = $("px");
  el.textContent = money(st2.index, 4);
  if (S.last !== null && st2.index !== S.last) {
    const up = st2.index > S.last;
    el.classList.toggle("up", up);
    el.classList.toggle("down", !up);
  }
  S.last = st2.index;

  $("lev").textContent = st2.leverage ? "up to " + st2.leverage + "×" : "—";
  if (S.token) quote();
  $("maint").textContent = st2.maintenance_bps ? (st2.maintenance_bps / 100).toFixed(2) + "%" : "—";
  $("conf").textContent = st2.confidence ?? "—";
  $("fills").textContent = st2.fills ?? "—";
  $("age").textContent = st2.age_seconds > 0 ? st2.age_seconds + "s ago" : "now";
  $("age").style.color = st2.age_seconds > 20 ? "var(--down)" : "var(--text-3)";

  /* Confidence is a gate, not a decoration: the VM refuses to open a position
     on a reading it cannot vouch for, so a market that will not trade should
     say so here rather than at the point of order. */
  const thin = (st2.confidence ?? 0) < 5000;
  $("dot").className = "dot " + (st2.halted || thin ? "off" : "on");
  $("connText").textContent = st2.halted
    ? "halted: " + (st2.halt_reason || "")
    : thin ? "oracle too thin to trade" : "live";
  $("foot").textContent =
    `${st2.pair} · seq ${st2.seq} · epoch ${st2.epoch} · ${st2.fills} fills · ` +
    `${st2.liquidations} liquidations · ${st2.quotes} maker quotes · ${st.traders} paper traders`;

  if (st2.index > 0) {
    S.ticks.push({ t: Date.now(), p: st2.index });
    if (S.ticks.length > S.max) S.ticks.shift();
    draw();
  }
}

function paintBalance() {
  if (!S.balance) return;
  const parts = [], alloc = [];
  for (const [asset, b] of Object.entries(S.balance)) {
    parts.push(`${money(b.free, 0)} ${asset.toUpperCase()}`);
    for (const [pair, v] of Object.entries(b.allocated || {})) {
      alloc.push(`${pair} ${money(v, 0)}`);
    }
  }
  $("free0").textContent = parts.join(" · ") || "—";
  $("alloc").textContent = alloc.length ? alloc.join(" · ") : "none";
}

$("pairSel").onchange = (e) => {
  S.pair = e.target.value;
  S.ticks = []; S.last = null;
  draw();
};

/* SSE for the price, and a poll for the position: the stream carries market
 * state that every viewer shares, while a position is one account's and is
 * fetched with its token rather than broadcast to everyone. */
function connect() {
  const es = new EventSource("/api/stream");
  es.onmessage = (e) => { try { paint(JSON.parse(e.data)); } catch {} };
  es.onerror = () => {
    $("dot").className = "dot off";
    $("connText").textContent = "reconnecting…";
  };
}
connect();

setInterval(async () => {
  if (!S.token) return;
  try { paint(await api("/api/state?token=" + encodeURIComponent(S.token))); }
  catch { /* the stream carries the market; this only adds the position */ }
}, 2500);

/* ---- chart --------------------------------------------------------------- */

const cv = $("chart"), ctx = cv.getContext("2d");
function draw() {
  const wrap = $("wrap"), W = wrap.clientWidth, H = wrap.clientHeight;
  const d = Math.min(devicePixelRatio || 1, 2);
  if (cv.width !== Math.round(W * d)) { cv.width = W * d; cv.height = H * d; }
  ctx.setTransform(d, 0, 0, d, 0, 0);
  ctx.clearRect(0, 0, W, H);
  if (S.ticks.length < 2) return;

  const cs = getComputedStyle(document.documentElement);
  const tok = (n) => cs.getPropertyValue(n).trim() || "#888";
  const M = { t: 14, r: 62, b: 16, l: 12 };
  const iw = W - M.l - M.r, ih = H - M.t - M.b;
  if (iw <= 0 || ih <= 0) return;

  let lo = Infinity, hi = -Infinity;
  for (const k of S.ticks) { lo = Math.min(lo, k.p); hi = Math.max(hi, k.p); }
  const mid = (lo + hi) / 2, floor = mid * 0.0008;
  if (hi - lo < floor) { lo = mid - floor / 2; hi = mid + floor / 2; }
  const pad = (hi - lo) * 0.18; lo -= pad; hi += pad;
  const X = (i) => M.l + (i / (S.ticks.length - 1)) * iw;
  const Y = (v) => M.t + ih - ((v - lo) / (hi - lo)) * ih;

  ctx.font = '10px "IBM Plex Mono", ui-monospace, monospace';
  ctx.textBaseline = "middle";
  for (let g = 0; g <= 4; g++) {
    const v = lo + ((hi - lo) * g) / 4, y = Math.round(Y(v)) + .5;
    ctx.strokeStyle = tok("--grid"); ctx.lineWidth = 1;
    ctx.beginPath(); ctx.moveTo(M.l, y); ctx.lineTo(M.l + iw, y); ctx.stroke();
    ctx.fillStyle = tok("--text-3"); ctx.textAlign = "left";
    ctx.fillText(money(v), M.l + iw + 8, y);
  }
  ctx.beginPath();
  S.ticks.forEach((k, i) => { const y = Y(k.p); i ? ctx.lineTo(X(i), y) : ctx.moveTo(X(i), y); });
  ctx.strokeStyle = tok("--ember"); ctx.lineWidth = 1.75; ctx.lineJoin = "round"; ctx.stroke();

  const last = S.ticks[S.ticks.length - 1];
  ctx.fillStyle = tok("--ember");
  ctx.fillRect(M.l + iw + 4, Y(last.p) - 8, M.r - 6, 16);
  ctx.fillStyle = tok("--paper");
  ctx.font = '600 10px "IBM Plex Mono", ui-monospace, monospace';
  ctx.fillText(money(last.p), M.l + iw + 8, Y(last.p));
}
addEventListener("resize", draw);

function log(msg, cls = "") {
  const el = $("log");
  const line = document.createElement("div");
  if (cls) line.className = cls;
  const t = new Date().toISOString().slice(11, 19);
  line.textContent = `${t}  ${msg}`;
  el.insertBefore(line, el.firstChild);
  while (el.childElementCount > 60) el.removeChild(el.lastChild);
}
