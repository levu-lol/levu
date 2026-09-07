/* The site's light field.
 *
 * Two passes from the card shader in zcash-pixel-nft, both of which are driven
 * there by `uView` -- where the phone is tilted. That input has no meaning on a
 * desktop page, so the cursor takes its place: it is where the reader is
 * looking, and the browser hands it to us as a continuous two-axis signal.
 *
 *   Starlight  runs the full page. Sparse, fixed structure, so scrolling never
 *              feels like the content is sliding over a moving texture. Each
 *              star carries its own twinkle phase, and the cursor is part of
 *              that phase -- moving the mouse makes the field glint.
 *   Caustics   runs only where the hero is, and fades out with it. Broad and
 *              low, it gives the top of the page weight the rest doesn't need.
 *
 * Everything here is faint on purpose. It sits behind a live price, an argument
 * about leverage, and a list of what is and isn't true yet; a background that
 * competes with any of those has failed. The brightest it gets is under the
 * cursor, and only while the cursor is moving. */

const VERT = `
attribute vec2 p;
void main(){ gl_Position = vec4(p, 0.0, 1.0); }`;

const FRAG = `
precision mediump float;
uniform vec2  uRes;
uniform float uTime;
uniform vec2  uCursor;   // 0..1 of the viewport, smoothed
uniform float uEnergy;   // 0..1, rises while the pointer moves
uniform vec3  uClick;    // xy in 0..1, z = age 0..1 (>=1 is spent)
uniform float uScroll;   // page offset in viewport heights
uniform float uHero;     // hero's bottom edge, 0..1 up from the viewport floor

float hash(vec2 p){ return fract(sin(dot(p, vec2(127.1, 311.7))) * 43758.5453); }

void main(){
  vec2 uv = gl_FragCoord.xy / uRes;
  float aspect = uRes.x / max(uRes.y, 1.0);
  vec2 a = vec2(uv.x * aspect, uv.y);      // square space, for honest distances
  vec2 c = vec2(uCursor.x * aspect, uCursor.y);
  vec2 view = (uCursor - 0.5);

  float near = exp(-distance(a, c) * 2.2); // how close this pixel is to the cursor

  vec3 col = vec3(0.0);

  /* --- Starlight ------------------------------------------------------ */
  /* Cells are square whatever the window shape, so the diffraction cross
     stays a cross. The field drifts slower than the page scrolls, which is
     what reads as distance. */
  vec2 sp = vec2(uv.x * aspect, uv.y + uScroll * 0.16) + view * 0.10;
  vec2 q = sp * 17.0;
  vec2 id = floor(q), f = fract(q) - 0.5;

  float gate = step(0.74, hash(id));
  float core = exp(-length(f) * 30.0);
  float cross = exp(-abs(f.x) * 62.0 - abs(f.y) * 7.0)
              + exp(-abs(f.y) * 62.0 - abs(f.x) * 7.0);
  float tw = 0.22 + 0.78 * pow(0.5 + 0.5 * sin(uTime * 1.15 + hash(id) * 30.0
                                               + view.x * 17.0 + view.y * 13.0), 2.0);

  // Cool white at rest; stars near the cursor pull toward the accent.
  vec3 star = mix(vec3(0.60, 0.76, 0.86), vec3(0.10, 0.95, 0.62), min(near * 1.3, 1.0));
  col += star * gate * (core + cross * 0.80) * tw * (0.16 + near * 0.55 + uEnergy * 0.10);

  /* --- Caustics, hero only -------------------------------------------- */
  float inHero = smoothstep(uHero - 0.14, uHero + 0.22, uv.y);
  if (inHero > 0.001) {
    vec2 p = uv * 9.0 + view * 2.2;
    p += vec2(sin(p.y + uTime * 0.45), cos(p.x - uTime * 0.35)) * 0.55;
    float net = abs(sin(p.x + p.y * 0.6 + uTime * 0.28) *
                    sin(p.y - p.x * 0.35 - uTime * 0.23));
    float caustic = pow(1.0 - smoothstep(0.0, 0.16, net), 3.0);
    col += vec3(0.0, 0.898, 0.549) * caustic * inHero
           * (0.026 + near * 0.32 + uEnergy * 0.05);
  }

  /* --- One ring per click --------------------------------------------- */
  if (uClick.z < 1.0) {
    vec2 k = vec2(uClick.x * aspect, uClick.y);
    float r = uClick.z * 0.85;
    float ring = exp(-pow((distance(a, k) - r) * 10.0, 2.0)) * (1.0 - uClick.z);
    col += vec3(0.29, 0.93, 0.80) * ring * 0.30;
  }

  // Keep clear of the sticky nav, which the field would otherwise run into.
  col *= 1.0 - smoothstep(0.90, 1.0, uv.y) * 0.9;

  /* Alpha carries the light, so the browser composites this ONTO the page's
     ground instead of replacing it. Writing 1.0 here paints an opaque black
     rectangle over --paper wherever the field is dark, which is every pixel
     that isn't a star. */
  float lum = clamp(max(col.r, max(col.g, col.b)), 0.0, 1.0);
  gl_FragColor = vec4(col, lum);
}`;

/* `hero` is optional: without it the caustics pass never runs and the page gets
 * the star field alone. That is the right default for any page that is an
 * instrument rather than an argument. */
export function siteBackground(canvas, hero) {
  if (!canvas) return;
  if (window.matchMedia("(prefers-reduced-motion: reduce)").matches) return;

  const gl = canvas.getContext("webgl", { alpha: true, antialias: false, depth: false });
  if (!gl) return; // a flat page is a fine page

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

  const U = {};
  for (const n of ["uRes", "uTime", "uCursor", "uEnergy", "uClick", "uScroll", "uHero"]) {
    U[n] = gl.getUniformLocation(prog, n);
  }

  /* Two cursors: where it is, and where the field believes it is. The field
   * chases, so a flicked mouse pulls the light after it instead of teleporting
   * it -- which is the whole difference between "follows the cursor" and
   * "is drawn at the cursor". */
  let target = { x: 0.5, y: 0.6 };
  let eased = { x: 0.5, y: 0.6 };
  let energy = 0;
  let click = { x: 0.5, y: 0.5, t: -1 };

  /* No visibility gate of our own. requestAnimationFrame already stops in a
   * hidden or backgrounded tab, which is the whole point of it; a second gate
   * on document.hidden only adds a way to latch off and never recover -- as it
   * did, silently, the first time this ran. */
  // Capped for the same reason the trading page caps its polling: a background
  // has no business asking for 120fps on a laptop battery.
  const FRAME = 1000 / 30;

  function resize() {
    const dpr = Math.min(window.devicePixelRatio || 1, 1.75);
    const w = Math.round(window.innerWidth * dpr);
    const h = Math.round(window.innerHeight * dpr);
    if (canvas.width !== w || canvas.height !== h) {
      canvas.width = w;
      canvas.height = h;
      gl.viewport(0, 0, w, h);
    }
  }

  // Viewport coordinates, y measured up from the floor to match gl_FragCoord.
  function point(e) {
    return {
      x: e.clientX / Math.max(window.innerWidth, 1),
      y: 1 - e.clientY / Math.max(window.innerHeight, 1),
    };
  }

  addEventListener("pointermove", (e) => {
    const p = point(e);
    energy = Math.min(1, energy + Math.hypot(p.x - target.x, p.y - target.y) * 6);
    target = p;
  }, { passive: true });

  addEventListener("pointerdown", (e) => {
    const p = point(e);
    click = { x: p.x, y: p.y, t: 0 };
  }, { passive: true });

  addEventListener("resize", resize, { passive: true });

  const start = performance.now();
  let last = 0;

  function frame(now) {
    requestAnimationFrame(frame);
    if (now - last < FRAME) return;
    last = now;

    resize();
    eased.x += (target.x - eased.x) * 0.06;
    eased.y += (target.y - eased.y) * 0.06;
    energy *= 0.94;
    if (click.t >= 0) click.t = Math.min(1.0001, click.t + 0.022);

    // Where the hero's bottom edge currently sits, in the shader's coordinates.
    // Zero once it has scrolled away, which retires the caustics with it.
    let heroEdge = 0;
    if (hero) {
      const b = hero.getBoundingClientRect().bottom;
      heroEdge = Math.max(0, Math.min(1, 1 - b / Math.max(window.innerHeight, 1)));
    } else {
      heroEdge = 2; // above every pixel: the pass is never reached
    }

    gl.uniform2f(U.uRes, canvas.width, canvas.height);
    gl.uniform1f(U.uTime, (now - start) / 1000);
    gl.uniform2f(U.uCursor, eased.x, eased.y);
    gl.uniform1f(U.uEnergy, Math.min(energy, 1));
    gl.uniform3f(U.uClick, click.x, click.y, click.t < 0 ? 1 : click.t);
    gl.uniform1f(U.uScroll, (window.scrollY || 0) / Math.max(window.innerHeight, 1));
    gl.uniform1f(U.uHero, heroEdge);
    gl.drawArrays(gl.TRIANGLES, 0, 3);
  }

  resize();
  requestAnimationFrame(frame);
}
