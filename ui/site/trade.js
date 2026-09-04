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
let fills = [];            // this session's fills, for the tape
let sort = { k: "volume_24h_usd", dir: -1 };
let wallet = null;         // { address, token }, once signed in
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
    const q = await server(`/api/quote?pair=${encodeURIComponent(keyOf(sel))}&qty=${qty}`);
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
    const out = await server("/api/order", {
      method: "POST",
      body: JSON.stringify({
        token: wallet.token, pair: keyOf(sel), side,
        qty, market: true,
      }),
    });
    fills.unshift({
      t: Date.now(), pair: keyOf(sel), side, qty,
      px: sel.px, seq: out.seq,
    });
    fills = fills.slice(0, 60);
    $("amt").value = "";
    note(`Order accepted at sequence ${out.seq}.`);
    await refresh();
  } catch (err) {
    note(err.message);
    paintSubmit(null, err.message);
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

  $("cash").textContent = wallet ? fmtUsd(freeBalance()) : "—";
  $("posval").textContent = wallet ? fmtUsd(allocated()) : "—";
  $("equity").textContent = wallet ? fmtUsd(freeBalance() + allocated() + unreal) : "—";
  $("upnl").textContent = wallet ? (unreal >= 0 ? "+" : "") + fmtUsd(unreal) : "—";
  $("upnl").className = "v num " + (unreal > 0 ? "up" : unreal < 0 ? "down" : "");
  $("rpnl").textContent = String(positions.length);

  renderTicket();
  renderPositions(positions);
  renderFills();
}

function renderTicket() {
  if (!sel) { $("tSym").textContent = ""; return; }
  const p = posOf(sel);
  $("tSym").textContent = sel.symbol + " / " + sel.quote.toUpperCase();
  $("tPx").textContent = fmtPx(sel.px);
  $("tChg").textContent = sel.state;
  $("tChg").className = "num " + (sel.state === "live" ? "up" : "mut");
  $("tDepth").textContent = fmtCompact(sel.book);
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
      `<td class="${pl > 0 ? "up" : pl < 0 ? "down" : "mut"}">${(pl >= 0 ? "+" : "") + fmtUsd(pl)}</td>`;
    tr.onclick = () => { sel = m; render(); refreshQuote(); };
    return tr;
  }));
  if (!positions.length) {
    const tr = document.createElement("tr");
    tr.innerHTML = `<td colspan="6" class="empty">No positions. Pick a market and open one.</td>`;
    $("posRows").replaceChildren(tr);
  }
}

function renderFills() {
  $("tradeNote").textContent = fills.length ? fills.length + " this session" : "none yet";
  $("tradeRows").replaceChildren(...fills.map((f) => {
    const tr = document.createElement("tr");
    const [sym, q] = f.pair.split("/");
    tr.innerHTML =
      `<td class="mut">${new Date(f.t).toLocaleTimeString("en-GB")}</td>` +
      `<td><span class="sym">${sym}</span><span class="q">${q}</span></td>` +
      `<td class="${f.side === "buy" ? "up" : "down"}">${f.side === "buy" ? "long" : "short"}</td>` +
      `<td>${fmtQty(f.qty)}</td><td>${fmtPx(f.px)}</td><td class="mut">seq ${f.seq}</td>`;
    return tr;
  }));
  if (!fills.length) {
    const tr = document.createElement("tr");
    tr.innerHTML = `<td colspan="6" class="empty">No orders yet.</td>`;
    $("tradeRows").replaceChildren(tr);
  }
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
let quoteTimer = null;
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
