/* The hero's light field.
 *
 * Adapted from the caustics pass in zcash-pixel-nft's card shader. That one is
 * driven by `uView` -- where the phone is tilted -- which has no meaning on a
 * desktop page. The cursor is the honest equivalent: it is where the reader is
 * looking, and it is already a continuous two-axis input the browser hands us
 * for free.
 *
 * Deliberately faint. This sits behind a live price and a headline, and a
 * background that competes with either is a background that has failed. The
 * brightest it ever gets is under the cursor, and only while the cursor is
 * moving; at rest it settles to almost nothing.
 *
 * Costs nothing when it should cost nothing: no context at all under
 * prefers-reduced-motion, and the loop stops whenever the hero scrolls out of
 * view or the tab is hidden. */

const VERT = `
attribute vec2 p;
void main(){ gl_Position = vec4(p, 0.0, 1.0); }`;

const FRAG = `
precision mediump float;
uniform vec2  uRes;
uniform float uTime;
uniform vec2  uCursor;   // 0..1, smoothed
uniform float uEnergy;   // 0..1, rises while the pointer moves
uniform vec3  uClick;    // xy in 0..1, z = age 0..1 (>=1 is spent)

void main(){
  vec2 uv = gl_FragCoord.xy / uRes;
  float aspect = uRes.x / max(uRes.y, 1.0);
  vec2 a = vec2(uv.x * aspect, uv.y);

  // The caustics field, offset by where the reader is pointing.
  vec2 q = uv * 9.0 + (uCursor - 0.5) * 2.2;
  q += vec2(sin(q.y + uTime * 0.45), cos(q.x - uTime * 0.35)) * 0.55;
  float net = abs(sin(q.x + q.y * 0.6 + uTime * 0.28) *
                  sin(q.y - q.x * 0.35 - uTime * 0.23));
  float caustic = pow(1.0 - smoothstep(0.0, 0.16, net), 3.0);

  // A soft lift under the cursor, so the light follows rather than just moves.
  vec2 c = vec2(uCursor.x * aspect, uCursor.y);
  float halo = exp(-distance(a, c) * 2.2);

  // One ring per click, expanding and fading.
  float ring = 0.0;
  if (uClick.z < 1.0) {
    vec2 k = vec2(uClick.x * aspect, uClick.y);
    float r = uClick.z * 0.85;
    ring = exp(-pow((distance(a, k) - r) * 10.0, 2.0)) * (1.0 - uClick.z);
  }

  // Levu's green, and a colder cyan for the ring so a click reads as an event
  // rather than more of the same.
  vec3 col = vec3(0.0, 0.898, 0.549) * caustic * (0.026 + halo * 0.32 + uEnergy * 0.05);
  col += vec3(0.29, 0.93, 0.80) * ring * 0.30;

  // Fade at both edges: the copy below must sit on a flat ground, and the top
  // must not run hot into the nav bar it butts against.
  col *= smoothstep(0.0, 0.55, uv.y) * (1.0 - smoothstep(0.72, 1.0, uv.y) * 0.85);
  gl_FragColor = vec4(col, 1.0);
}`;

export function heroBackground(canvas, host) {
  if (!canvas || !host) return;
  if (window.matchMedia("(prefers-reduced-motion: reduce)").matches) return;

  const gl = canvas.getContext("webgl", { alpha: true, antialias: false, depth: false });
  if (!gl) return; // a flat hero is a fine hero

  const compile = (type, src) => {
    const s = gl.createShader(type);
    gl.shaderSource(s, src);
    gl.compileShader(s);
    return gl.getShaderParameter(s, gl.COMPILE_STATUS) ? s : null;
  };
  const vs = compile(gl.VERTEX_SHADER, VERT);
  const fs = compile(gl.FRAGMENT_SHADER, FRAG);
  if (!vs || !fs) return;

  const prog = gl.createProgram();
  gl.attachShader(prog, vs);
  gl.attachShader(prog, fs);
  gl.linkProgram(prog);
  if (!gl.getProgramParameter(prog, gl.LINK_STATUS)) return;
  gl.useProgram(prog);

  const buf = gl.createBuffer();
  gl.bindBuffer(gl.ARRAY_BUFFER, buf);
  gl.bufferData(gl.ARRAY_BUFFER, new Float32Array([-1, -1, 3, -1, -1, 3]), gl.STATIC_DRAW);
  const loc = gl.getAttribLocation(prog, "p");
  gl.enableVertexAttribArray(loc);
  gl.vertexAttribPointer(loc, 2, gl.FLOAT, false, 0, 0);

  const U = {
    res: gl.getUniformLocation(prog, "uRes"),
    time: gl.getUniformLocation(prog, "uTime"),
    cursor: gl.getUniformLocation(prog, "uCursor"),
    energy: gl.getUniformLocation(prog, "uEnergy"),
    click: gl.getUniformLocation(prog, "uClick"),
  };

  /* Two cursors: where it is, and where the field believes it is. The field
   * chases, so a flicked mouse pulls the light after it instead of teleporting
   * it -- which is the whole difference between "follows the cursor" and
   * "is drawn at the cursor". */
  let target = { x: 0.5, y: 0.55 };
  let eased = { x: 0.5, y: 0.55 };
  let energy = 0;
  let click = { x: 0.5, y: 0.5, t: -1 };
  let visible = true;
  let raf = 0;

  // Capped for the same reason the trading page caps its polling: a hero
  // background has no business asking for 120fps on a laptop battery.
  const FPS = 30;
  const FRAME = 1000 / FPS;

  function resize() {
    const dpr = Math.min(window.devicePixelRatio || 1, 1.75);
    const w = Math.round(host.clientWidth * dpr);
    const h = Math.round(host.clientHeight * dpr);
    if (canvas.width !== w || canvas.height !== h) {
      canvas.width = w;
      canvas.height = h;
      gl.viewport(0, 0, w, h);
    }
  }

  function point(e) {
    const r = host.getBoundingClientRect();
    const x = (e.clientX - r.left) / Math.max(r.width, 1);
    const y = 1 - (e.clientY - r.top) / Math.max(r.height, 1);
    return { x: Math.min(Math.max(x, -0.5), 1.5), y: Math.min(Math.max(y, -0.5), 1.5) };
  }

  host.addEventListener("pointermove", (e) => {
    const p = point(e);
    energy = Math.min(1, energy + Math.hypot(p.x - target.x, p.y - target.y) * 6);
    target = p;
  }, { passive: true });

  host.addEventListener("pointerdown", (e) => {
    const p = point(e);
    click = { x: p.x, y: p.y, t: 0 };
  }, { passive: true });

  host.addEventListener("pointerleave", () => {
    target = { x: 0.5, y: 0.55 };
  }, { passive: true });

  const start = performance.now();
  let last = 0;

  function frame(now) {
    raf = requestAnimationFrame(frame);
    if (!visible || now - last < FRAME) return;
    last = now;

    resize();
    eased.x += (target.x - eased.x) * 0.06;
    eased.y += (target.y - eased.y) * 0.06;
    energy *= 0.94;
    if (click.t >= 0) click.t = Math.min(1.0001, click.t + 0.022);

    gl.uniform2f(U.res, canvas.width, canvas.height);
    gl.uniform1f(U.time, (now - start) / 1000);
    gl.uniform2f(U.cursor, eased.x, eased.y);
    gl.uniform1f(U.energy, Math.min(energy, 1));
    gl.uniform3f(U.click, click.x, click.y, click.t < 0 ? 1 : click.t);
    gl.drawArrays(gl.TRIANGLES, 0, 3);
  }

  if ("IntersectionObserver" in window) {
    new IntersectionObserver(([e]) => { visible = e.isIntersecting; })
      .observe(host);
  }
  document.addEventListener("visibilitychange", () => {
    visible = !document.hidden;
  });
  window.addEventListener("resize", resize, { passive: true });

  resize();
  raf = requestAnimationFrame(frame);
}
