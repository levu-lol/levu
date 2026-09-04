/* chart.js -- an interactive price chart for the lane terminal.
 *
 * What this draws is NOT a trade chart, and the distinction matters enough to
 * state at the top of the file. There is no order flow here: every point is a
 * sample of Uniswap pool state, aggregated into an index by the same weighting
 * the Go control plane uses. So a "candle" is the open/high/low/close of the
 * INDEX over an interval, and there is no volume, because nothing traded. The
 * pane below the price is executable depth -- a level, not a flow -- which is
 * the honest analogue and is what actually constrains the lane's open interest.
 *
 * Interaction model follows the conventions a trader already has in their
 * hands, because a chart that invents its own is a chart you have to learn:
 *
 *   wheel over plot     zoom time about the cursor
 *   drag plot           pan
 *   drag price axis     stretch/compress price
 *   drag time axis      stretch/compress time
 *   double-click        refit both axes and resume following
 *
 * Following is sticky and self-cancelling: the view tracks the newest candle
 * until you pan away from it, and then stops, because a chart that yanks itself
 * back to the right edge while you are reading history is useless.
 */

export const TIMEFRAMES = [
  { label: "5s", ms: 5_000 },
  { label: "15s", ms: 15_000 },
  { label: "1m", ms: 60_000 },
  { label: "5m", ms: 300_000 },
  { label: "15m", ms: 900_000 },
];

/* Bucket raw samples into OHLC. Depth and mark are levels, so they take the
 * last value in the bucket rather than an extreme or a sum. */
export function buildCandles(ticks, tfMs) {
  const out = [];
  let cur = null;
  for (const t of ticks) {
    const bucket = Math.floor(t.t / tfMs) * tfMs;
    if (cur === null || cur.t !== bucket) {
      cur = {
        t: bucket, o: t.index, h: t.index, l: t.index, c: t.index,
        mark: t.mark, depth: t.depth || 0, conf: t.conf || 0, n: 1,
      };
      out.push(cur);
    } else {
      if (t.index > cur.h) cur.h = t.index;
      if (t.index < cur.l) cur.l = t.index;
      cur.c = t.index;
      cur.mark = t.mark;
      cur.depth = t.depth || cur.depth;
      cur.conf = t.conf || cur.conf;
      cur.n++;
    }
  }
  return out;
}

/* Gridlines on round numbers. A grid on arbitrary values makes every reading
 * an arithmetic problem. */
function niceTicks(lo, hi, want) {
  const span = hi - lo;
  if (!(span > 0) || !isFinite(span)) return [];
  const raw = span / Math.max(1, want);
  const mag = Math.pow(10, Math.floor(Math.log10(raw)));
  const n = raw / mag;
  const step = (n <= 1 ? 1 : n <= 2 ? 2 : n <= 5 ? 5 : 10) * mag;
  const out = [];
  for (let v = Math.ceil(lo / step) * step; v <= hi + step * 1e-9; v += step) out.push(v);
  return out;
}

const clamp = (v, a, b) => (v < a ? a : v > b ? b : v);
const dpr = () => Math.min(window.devicePixelRatio || 1, 2);

export class Chart {
  constructor(canvas, opts = {}) {
    this.cv = canvas;
    this.ctx = canvas.getContext("2d");
    this.onHover = opts.onHover || (() => {});
    this.onView = opts.onView || (() => {});
    this.bandBps = opts.bandBps ?? 50;
    this.fmt = opts.fmt || ((v) => v.toFixed(2));
    this.fmtDepth = opts.fmtDepth || ((v) => String(Math.round(v)));

    this.ticks = [];
    this.candles = [];
    this.tf = 5_000;
    this.type = "candle";
    this.showBand = true;
    this.showMark = true;
    this.showDepth = true;

    /* Time viewport lives in candle-index space and is fractional, so zoom is
     * continuous rather than stepping by whole candles. */
    this.view = { from: 0, to: 60 };
    this.following = true;
    /* Until someone actually grabs the chart, the view fits whatever data has
     * arrived. A fresh page has three candles; showing them squeezed against
     * the right edge of a 60-wide window is a worse default than showing three
     * candles. Double-click hands control back. */
    this.autoFit = true;
    this.yManual = null;          /* null => fit to what is visible */
    this.cursor = null;
    this.drag = null;

    this.M = { t: 10, r: 68, b: 22, l: 10 };
    this.depthH = 54;

    this._bind();
    this._ro = new ResizeObserver(() => this.draw());
    this._ro.observe(canvas.parentElement || canvas);

    /* The viewer can flip the OS theme, or the host can stamp data-theme, at
     * any time; either way the cached colours are stale. */
    const mq = window.matchMedia("(prefers-color-scheme: dark)");
    mq.addEventListener?.("change", () => this.invalidateTheme());
    new MutationObserver(() => this.invalidateTheme())
      .observe(document.documentElement, { attributes: true, attributeFilter: ["data-theme"] });
  }

  /* ---- data ------------------------------------------------------------- */

  setTicks(ticks) {
    this.ticks = ticks;
    const prev = this.candles.length;
    this.candles = buildCandles(ticks, this.tf);
    if (this.autoFit) this._fit();
    else if (this.following) this._snapRight();
    this.draw();
  }

  setTimeframe(ms) {
    if (ms === this.tf) return;
    this.tf = ms;
    this.candles = buildCandles(this.ticks, ms);
    this.yManual = null;
    this.following = true;
    /* A timeframe change rescales the x axis under you, so whatever span you
     * had chosen no longer means the same thing. Refit rather than preserve. */
    this._fit();
    this.draw();
    this.onView({ following: true });
  }

  setType(t) { this.type = t; this.draw(); }
  setBand(v) { this.showBand = v; this.draw(); }
  setMark(v) { this.showMark = v; this.draw(); }
  setDepth(v) { this.showDepth = v; this.draw(); }

  resetView() {
    this.yManual = null;
    this.following = true;
    this.autoFit = true;
    this._fit();
    this.draw();
    this.onView({ following: true });
  }

  /* Fit the whole series with a little air on the right for the live candle. */
  _fit() {
    const n = this.candles.length;
    const span = Math.max(12, n * 1.12);
    this.view.to = n + span * 0.06;
    this.view.from = this.view.to - span;
    this.following = true;
  }

  goLive() { this.following = true; this._snapRight(); this.draw(); this.onView({ following: true }); }

  _snapRight() {
    const span = Math.max(4, this.view.to - this.view.from);
    const n = this.candles.length;
    this.view.to = n + span * 0.06;
    this.view.from = this.view.to - span;
  }

  /* ---- geometry --------------------------------------------------------- */

  _layout() {
    const wrap = this.cv.parentElement;
    const W = wrap.clientWidth, H = wrap.clientHeight;
    const d = dpr();
    if (this.cv.width !== Math.round(W * d) || this.cv.height !== Math.round(H * d)) {
      this.cv.width = Math.round(W * d);
      this.cv.height = Math.round(H * d);
    }
    this.ctx.setTransform(d, 0, 0, d, 0, 0);
    const dh = this.showDepth ? this.depthH : 0;
    const iw = W - this.M.l - this.M.r;
    const ih = H - this.M.t - this.M.b - dh;
    return { W, H, iw, ih, dh };
  }

  _xOf(i, L) { return this.M.l + ((i - this.view.from) / (this.view.to - this.view.from)) * L.iw; }
  _iAt(x, L) { return this.view.from + ((x - this.M.l) / L.iw) * (this.view.to - this.view.from); }

  /* Price domain: what the eye should be given. Candle extremes when fitting,
   * a floor so a still market's last cent is not stretched into a trend, and
   * the band only when it actually reaches into view. */
  _yDomain(vis) {
    if (this.yManual) return this.yManual;
    let lo = Infinity, hi = -Infinity;
    for (const c of vis) {
      if (c.l < lo) lo = c.l;
      if (c.h > hi) hi = c.h;
      if (this.showMark) { if (c.mark < lo) lo = c.mark; if (c.mark > hi) hi = c.mark; }
    }
    if (!isFinite(lo)) return { lo: 0, hi: 1 };
    const mid = (lo + hi) / 2;
    const MIN = mid * (8 / 10_000);
    if (hi - lo < MIN) { lo = mid - MIN / 2; hi = mid + MIN / 2; }
    const pad = (hi - lo) * 0.16;
    return { lo: lo - pad, hi: hi + pad };
  }

  /* ---- draw ------------------------------------------------------------- */

  /* Crosshair movement redraws the chart, so resolving eight custom properties
   * through getComputedStyle per frame is a real cost for values that change
   * only when the theme does. Cache them, and let the theme invalidate. */
  _tokens() {
    if (this._tok) return this._tok;
    const cs = getComputedStyle(document.documentElement);
    const g = (n) => cs.getPropertyValue(n).trim() || "#888";
    this._tok = {
      ember: g("--ember"), t2: g("--text-2"), t3: g("--text-3"),
      grid: g("--grid"), up: g("--up"), down: g("--down"),
      paper: g("--paper"), rule: g("--rule"),
    };
    return this._tok;
  }

  invalidateTheme() { this._tok = null; this.draw(); }

  draw() {
    const ctx = this.ctx, L = this._layout();
    ctx.clearRect(0, 0, L.W, L.H);
    if (L.iw <= 0 || L.ih <= 0) return;

    const C = this._tokens();

    const n = this.candles.length;
    if (n === 0) return;
    const i0 = clamp(Math.floor(this.view.from) - 1, 0, Math.max(0, n - 1));
    const i1 = clamp(Math.ceil(this.view.to) + 1, 0, n);
    const vis = this.candles.slice(i0, i1);
    if (vis.length === 0) return;

    const { lo, hi } = this._yDomain(vis);
    const Y = (v) => this.M.t + L.ih - ((v - lo) / (hi - lo)) * L.ih;
    const vAt = (y) => lo + ((this.M.t + L.ih - y) / L.ih) * (hi - lo);
    this._Y = Y; this._vAt = vAt; this._L = L; this._dom = { lo, hi };

    ctx.font = '10px "IBM Plex Mono", ui-monospace, monospace';
    ctx.textBaseline = "middle";

    /* price grid + axis */
    const last = this.candles[n - 1];
    const lastY = Y(last.c);
    for (const v of niceTicks(lo, hi, Math.max(2, Math.round(L.ih / 46)))) {
      const y = Math.round(Y(v)) + .5;
      ctx.strokeStyle = C.grid; ctx.lineWidth = 1;
      ctx.beginPath(); ctx.moveTo(this.M.l, y); ctx.lineTo(this.M.l + L.iw, y); ctx.stroke();
      if (Math.abs(y - lastY) < 13) continue;   /* the live tag owns its row */
      ctx.fillStyle = C.t3; ctx.textAlign = "left";
      ctx.fillText(this.fmt(v), this.M.l + L.iw + 8, y);
    }

    /* time grid */
    const span = this.view.to - this.view.from;
    const stepC = Math.max(1, Math.round(span / Math.max(2, L.iw / 92)));
    ctx.textAlign = "center";
    for (let i = Math.ceil(this.view.from / stepC) * stepC; i < this.view.to; i += stepC) {
      if (i < 0 || i >= n) continue;
      const x = Math.round(this._xOf(i + .5, L)) + .5;
      ctx.strokeStyle = C.grid; ctx.lineWidth = 1;
      ctx.beginPath(); ctx.moveTo(x, this.M.t); ctx.lineTo(x, this.M.t + L.ih); ctx.stroke();
      ctx.fillStyle = C.t3;
      ctx.fillText(hhmmss(this.candles[i].t, this.tf), x, this.M.t + L.ih + 11);
    }

    ctx.save();
    ctx.beginPath(); ctx.rect(this.M.l, this.M.t, L.iw, L.ih); ctx.clip();

    /* band */
    if (this.showBand) {
      const k = this.bandBps / 10_000;
      ctx.beginPath();
      vis.forEach((c, j) => { const x = this._xOf(i0 + j + .5, L), y = Y(c.c * (1 + k)); j ? ctx.lineTo(x, y) : ctx.moveTo(x, y); });
      for (let j = vis.length - 1; j >= 0; j--) ctx.lineTo(this._xOf(i0 + j + .5, L), Y(vis[j].c * (1 - k)));
      ctx.closePath();
      ctx.fillStyle = C.ember; ctx.globalAlpha = .10; ctx.fill(); ctx.globalAlpha = 1;
    }

    /* price */
    const cw = Math.max(1, (L.iw / span) * 0.62);
    if (this.type === "candle") {
      vis.forEach((c, j) => {
        const x = this._xOf(i0 + j + .5, L);
        const up = c.c >= c.o;
        ctx.strokeStyle = up ? C.up : C.down;
        ctx.fillStyle = up ? C.up : C.down;
        ctx.lineWidth = 1;
        const xw = Math.round(x) + .5;
        ctx.beginPath(); ctx.moveTo(xw, Y(c.h)); ctx.lineTo(xw, Y(c.l)); ctx.stroke();
        const yo = Y(c.o), yc = Y(c.c);
        const top = Math.min(yo, yc), h = Math.max(1, Math.abs(yc - yo));
        if (cw <= 2) { ctx.fillRect(xw - .5, top, 1, h); }
        else ctx.fillRect(x - cw / 2, top, cw, h);
      });
    } else {
      ctx.beginPath();
      vis.forEach((c, j) => { const x = this._xOf(i0 + j + .5, L), y = Y(c.c); j ? ctx.lineTo(x, y) : ctx.moveTo(x, y); });
      if (this.type === "area") {
        ctx.lineTo(this._xOf(i0 + vis.length - 1 + .5, L), this.M.t + L.ih);
        ctx.lineTo(this._xOf(i0 + .5, L), this.M.t + L.ih);
        ctx.closePath();
        ctx.fillStyle = C.ember; ctx.globalAlpha = .13; ctx.fill(); ctx.globalAlpha = 1;
        ctx.beginPath();
        vis.forEach((c, j) => { const x = this._xOf(i0 + j + .5, L), y = Y(c.c); j ? ctx.lineTo(x, y) : ctx.moveTo(x, y); });
      }
      ctx.strokeStyle = C.ember; ctx.lineWidth = 1.75; ctx.lineJoin = "round"; ctx.stroke();
    }

    /* mark */
    if (this.showMark) {
      ctx.beginPath();
      vis.forEach((c, j) => { const x = this._xOf(i0 + j + .5, L), y = Y(c.mark); j ? ctx.lineTo(x, y) : ctx.moveTo(x, y); });
      ctx.strokeStyle = C.t2; ctx.lineWidth = 1; ctx.setLineDash([3, 3]); ctx.stroke(); ctx.setLineDash([]);
    }
    ctx.restore();

    /* depth pane -- a level, not a flow, so it is drawn as a step area */
    if (this.showDepth) {
      const dy = this.M.t + L.ih + this.M.b, dh = L.dh - 6;
      let dmax = 0;
      for (const c of vis) if (c.depth > dmax) dmax = c.depth;
      if (dmax > 0) {
        ctx.save();
        ctx.beginPath(); ctx.rect(this.M.l, dy, L.iw, dh); ctx.clip();
        ctx.beginPath();
        ctx.moveTo(this._xOf(i0 + .5, L), dy + dh);
        vis.forEach((c, j) => { ctx.lineTo(this._xOf(i0 + j + .5, L), dy + dh - (c.depth / dmax) * dh * .88); });
        ctx.lineTo(this._xOf(i0 + vis.length - 1 + .5, L), dy + dh);
        ctx.closePath();
        ctx.fillStyle = C.t2; ctx.globalAlpha = .16; ctx.fill(); ctx.globalAlpha = 1;
        ctx.restore();
        ctx.strokeStyle = C.rule; ctx.lineWidth = 1;
        ctx.beginPath(); ctx.moveTo(this.M.l, dy - .5); ctx.lineTo(this.M.l + L.iw, dy - .5); ctx.stroke();
        ctx.fillStyle = C.t3; ctx.textAlign = "left"; ctx.font = '9px "IBM Plex Mono", ui-monospace, monospace';
        ctx.fillText("DEPTH", this.M.l + 3, dy + 8);
        ctx.fillText(this.fmtDepth(dmax), this.M.l + L.iw + 8, dy + 8);
      }
    }

    /* live price tag */
    ctx.font = '10px "IBM Plex Mono", ui-monospace, monospace';
    const tagY = clamp(lastY, this.M.t + 6, this.M.t + L.ih - 6);
    const up = last.c >= last.o;
    ctx.fillStyle = up ? C.up : C.down;
    ctx.fillRect(this.M.l + L.iw + 4, tagY - 8, this.M.r - 6, 16);
    ctx.fillStyle = C.paper; ctx.textAlign = "left";
    ctx.font = '600 10px "IBM Plex Mono", ui-monospace, monospace';
    ctx.fillText(this.fmt(last.c), this.M.l + L.iw + 8, tagY);
    ctx.save();
    ctx.beginPath(); ctx.rect(this.M.l, this.M.t, L.iw, L.ih); ctx.clip();
    ctx.strokeStyle = up ? C.up : C.down; ctx.globalAlpha = .5;
    ctx.setLineDash([2, 3]); ctx.lineWidth = 1;
    ctx.beginPath(); ctx.moveTo(this.M.l, Math.round(lastY) + .5); ctx.lineTo(this.M.l + L.iw, Math.round(lastY) + .5); ctx.stroke();
    ctx.setLineDash([]); ctx.globalAlpha = 1; ctx.restore();

    /* crosshair */
    if (this.cursor) {
      const { x, y } = this.cursor;
      if (x >= this.M.l && x <= this.M.l + L.iw && y >= this.M.t && y <= this.M.t + L.ih + (this.showDepth ? L.dh + this.M.b : 0)) {
        ctx.strokeStyle = C.t3; ctx.globalAlpha = .65; ctx.setLineDash([2, 3]); ctx.lineWidth = 1;
        ctx.beginPath(); ctx.moveTo(Math.round(x) + .5, this.M.t); ctx.lineTo(Math.round(x) + .5, this.M.t + L.ih + (this.showDepth ? L.dh + this.M.b : 0)); ctx.stroke();
        if (y <= this.M.t + L.ih) {
          ctx.beginPath(); ctx.moveTo(this.M.l, Math.round(y) + .5); ctx.lineTo(this.M.l + L.iw, Math.round(y) + .5); ctx.stroke();
        }
        ctx.setLineDash([]); ctx.globalAlpha = 1;

        if (y <= this.M.t + L.ih) {
          ctx.fillStyle = C.t2;
          ctx.fillRect(this.M.l + L.iw + 4, y - 8, this.M.r - 6, 16);
          ctx.fillStyle = C.paper; ctx.textAlign = "left";
          ctx.fillText(this.fmt(vAt(y)), this.M.l + L.iw + 8, y);
        }
        const ci = Math.floor(this._iAt(x, L));
        if (ci >= 0 && ci < n) {
          const label = hhmmss(this.candles[ci].t, this.tf);
          ctx.textAlign = "center";
          const w = ctx.measureText(label).width + 10;
          ctx.fillStyle = C.t2;
          ctx.fillRect(x - w / 2, this.M.t + L.ih + 2, w, 15);
          ctx.fillStyle = C.paper;
          ctx.fillText(label, x, this.M.t + L.ih + 10);
        }
      }
    }
  }

  /* ---- interaction ------------------------------------------------------ */

  _pos(e) {
    const r = this.cv.getBoundingClientRect();
    return { x: e.clientX - r.left, y: e.clientY - r.top };
  }
  _zone(p) {
    const L = this._L; if (!L) return "plot";
    if (p.x > this.M.l + L.iw) return "yaxis";
    if (p.y > this.M.t + L.ih && p.y <= this.M.t + L.ih + this.M.b) return "xaxis";
    return "plot";
  }

  _bind() {
    const cv = this.cv;

    cv.addEventListener("wheel", (e) => {
      e.preventDefault();
      const L = this._L; if (!L) return;
      const p = this._pos(e);
      this.autoFit = false;
      const f = e.deltaY > 0 ? 1.12 : 1 / 1.12;
      const anchor = this._iAt(clamp(p.x, this.M.l, this.M.l + L.iw), L);
      const span = clamp((this.view.to - this.view.from) * f, 6, Math.max(60, this.candles.length * 3));
      const ratio = (anchor - this.view.from) / (this.view.to - this.view.from);
      this.view.from = anchor - span * ratio;
      this.view.to = this.view.from + span;
      this._clampX();
      this._checkFollow();
      this.draw();
    }, { passive: false });

    cv.addEventListener("mousedown", (e) => {
      const p = this._pos(e);
      this.autoFit = false;
      this.drag = { zone: this._zone(p), x: p.x, y: p.y, view: { ...this.view }, dom: this._dom ? { ...this._dom } : null };
      cv.style.cursor = this.drag.zone === "plot" ? "grabbing" : this.drag.zone === "yaxis" ? "ns-resize" : "ew-resize";
    });

    window.addEventListener("mouseup", () => {
      if (!this.drag) return;
      this.drag = null;
      cv.style.cursor = "crosshair";
    });

    cv.addEventListener("mousemove", (e) => {
      const p = this._pos(e);
      const L = this._L;
      if (this.drag && L) {
        const dx = p.x - this.drag.x, dy = p.y - this.drag.y;
        if (this.drag.zone === "plot") {
          const per = (this.drag.view.to - this.drag.view.from) / L.iw;
          this.view.from = this.drag.view.from - dx * per;
          this.view.to = this.drag.view.to - dx * per;
          this._clampX();
          if (this.yManual && this.drag.dom) {
            const vper = (this.drag.dom.hi - this.drag.dom.lo) / L.ih;
            this.yManual = { lo: this.drag.dom.lo + dy * vper, hi: this.drag.dom.hi + dy * vper };
          }
          this._checkFollow();
        } else if (this.drag.zone === "yaxis" && this.drag.dom) {
          /* Pull down to compress, up to expand, about the middle. */
          const f = clamp(1 + dy / 180, 0.25, 4);
          const mid = (this.drag.dom.lo + this.drag.dom.hi) / 2;
          const half = ((this.drag.dom.hi - this.drag.dom.lo) / 2) * f;
          this.yManual = { lo: mid - half, hi: mid + half };
        } else if (this.drag.zone === "xaxis") {
          const f = clamp(1 - dx / 220, 0.25, 4);
          const span = clamp((this.drag.view.to - this.drag.view.from) * f, 6, Math.max(60, this.candles.length * 3));
          this.view.to = this.drag.view.to;
          this.view.from = this.view.to - span;
          this._clampX();
          this._checkFollow();
        }
        this.cursor = p;
        this.draw();
        return;
      }
      cv.style.cursor = { plot: "crosshair", yaxis: "ns-resize", xaxis: "ew-resize" }[this._zone(p)];
      this.cursor = p;
      this.draw();
      this._emitHover(p);
    });

    cv.addEventListener("mouseleave", () => {
      this.cursor = null; this.draw(); this.onHover(null);
    });

    cv.addEventListener("dblclick", () => this.resetView());
  }

  _emitHover(p) {
    const L = this._L; if (!L) return this.onHover(null);
    if (p.x < this.M.l || p.x > this.M.l + L.iw || p.y > this.M.t + L.ih) return this.onHover(null);
    const i = Math.floor(this._iAt(p.x, L));
    if (i < 0 || i >= this.candles.length) return this.onHover(null);
    this.onHover({ candle: this.candles[i], price: this._vAt ? this._vAt(p.y) : null });
  }

  /* Empty space is not data. Panning is allowed a little past each end -- so
   * the newest candle is not welded to the axis, and so you can see where the
   * series starts -- but not into an unbounded void in either direction. */
  _clampX() {
    const n = this.candles.length, span = this.view.to - this.view.from;
    const maxTo = n + span * 0.35;
    const minFrom = -span * 0.35;
    if (this.view.to > maxTo) { this.view.to = maxTo; this.view.from = maxTo - span; }
    if (this.view.from < minFrom) { this.view.from = minFrom; this.view.to = minFrom + span; }
  }

  /* Following is a consequence of where the view is, not a mode you toggle:
   * if the right edge is in sight, keep up; if you have panned off it, stop. */
  _checkFollow() {
    const wasFollowing = this.following;
    this.following = this.view.to >= this.candles.length - 0.5;
    if (wasFollowing !== this.following) this.onView({ following: this.following });
  }
}

function hhmmss(ms, tf) {
  const d = new Date(ms);
  const p = (v) => String(v).padStart(2, "0");
  return tf >= 60_000
    ? `${p(d.getUTCHours())}:${p(d.getUTCMinutes())}`
    : `${p(d.getUTCMinutes())}:${p(d.getUTCSeconds())}`;
}
