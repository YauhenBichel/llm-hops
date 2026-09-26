/* llm-hops page. Plain JavaScript, no build step. Every piece of text from the server goes through
   textContent or an attribute, never innerHTML. */
(() => {
  "use strict";
  const $ = (s, el = document) => el.querySelector(s);
  const $$ = (s, el = document) => Array.from(el.querySelectorAll(s));
  const el = (tag, attrs = {}, ...kids) => {
    const n = document.createElement(tag);
    for (const [k, v] of Object.entries(attrs)) {
      if (k === "class") n.className = v;
      else if (k === "text") n.textContent = v;
      else if (k.startsWith("on")) n.addEventListener(k.slice(2), v);
      else if (v !== null && v !== undefined) n.setAttribute(k, v);
    }
    for (const k of kids) if (k !== null && k !== undefined) n.append(k);
    return n;
  };
  const SVG = "http://www.w3.org/2000/svg";
  const svg = (tag, attrs = {}, ...kids) => {
    const n = document.createElementNS(SVG, tag);
    for (const [k, v] of Object.entries(attrs)) {
      if (k === "text") n.textContent = v;
      else if (k.startsWith("on")) n.addEventListener(k.slice(2), v);
      else n.setAttribute(k, v);
    }
    for (const k of kids) if (k) n.append(k);
    return n;
  };

  // ---- state ------------------------------------------------------------------------------------------
  const state = {
    view: "flow", windowMs: 3600000, q: "", paused: false, filters: { model: "", client: "", service: "", status: "", min: "" },
    traces: [], selected: null, flow: null, stats: null, es: null, theme: null, refreshTimer: null,
  };
  try { const t = localStorage.getItem("hops.theme"); if (t) { state.theme = t; document.documentElement.dataset.theme = t; } } catch (e) { /* private window */ }
  try { const v = localStorage.getItem("hops.view"); if (v) state.view = v; } catch (e) { /* ignore */ }
  // the address bar: #flow, #traces, #stats, and #trace=<id> opens one trace
  const hashView = (location.hash.match(/^#(flow|traces|stats)/) || [])[1];
  if (hashView) state.view = hashView;
  const hashTrace = (location.hash.match(/trace=([0-9a-f-]+)/) || [])[1];
  const hashWindow = Number((location.hash.match(/w=(\d+)/) || [])[1]);
  if (hashWindow) state.windowMs = hashWindow;
  // a token in the address (#token=...) becomes the cookie the server checks, then leaves the address
  const hashToken = (location.hash.match(/token=([^&]+)/) || [])[1];
  const session = hashToken ? fetch("/api/v1/session", { method: "POST", headers: { authorization: `Bearer ${decodeURIComponent(hashToken)}` } })
    .then(() => history.replaceState(null, "", location.hash.replace(/&?token=[^&]+/, "") || "#flow")).catch(() => {}) : Promise.resolve();

  const fmtMs = (ms) => ms == null ? "" : ms < 1000 ? `${Math.round(ms)} ms` : ms < 60000 ? `${(ms / 1000).toFixed(ms < 10000 ? 2 : 1)} s` : `${(ms / 60000).toFixed(1)} min`;
  const fmtTime = (ms) => { const d = new Date(ms); return d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit" }); };
  const fmtDate = (ms) => new Date(ms).toLocaleString([], { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit", second: "2-digit" });
  const kind = (service) => service === "router" || service === "harness" ? "router" : service === "gateway" ? "gateway" :
    ["ollama", "cpu", "audio", "speech", "comfyui", "backend", "model", "llama", "vllm", "anthropic", "openai", "cloud", "claude", "gemini", "mistral"].some((k) => service.startsWith(k)) ? "backend" : "other";
  const colorOf = (service) => `var(--${kind(service)})`;
  const modelOf = (t) => t.attrs.model || t.attrs.requested_model || "?";

  const api = async (path, params = {}) => {
    const u = new URL(path, location.origin);
    for (const [k, v] of Object.entries(params)) if (v !== "" && v !== null && v !== undefined) u.searchParams.set(k, v);
    const r = await fetch(u);
    if (!r.ok) throw new Error(`${r.status} ${path}`);
    return r.json();
  };
  const windowParams = () => ({ since: Date.now() - state.windowMs, until: Date.now() + 60000 });

  // ---- tooltip ----------------------------------------------------------------------------------------
  const tip = $("#tip");
  const showTip = (ev, lines) => {
    tip.replaceChildren(...lines.map((l, i) => {
      const d = el("div");
      if (Array.isArray(l)) { d.append(el("b", { text: l[0] }), " ", l[1]); } else d.textContent = l;
      return d;
    }));
    tip.hidden = false;
    moveTip(ev);
  };
  const moveTip = (ev) => {
    const x = Math.min(ev.clientX + 14, window.innerWidth - tip.offsetWidth - 8);
    const y = Math.min(ev.clientY + 14, window.innerHeight - tip.offsetHeight - 8);
    tip.style.left = `${x}px`; tip.style.top = `${y}px`;
  };
  const hideTip = () => { tip.hidden = true; };
  const withTip = (node, lines) => {
    node.addEventListener("mouseenter", (e) => showTip(e, typeof lines === "function" ? lines() : lines));
    node.addEventListener("mousemove", moveTip);
    node.addEventListener("mouseleave", hideTip);
    return node;
  };

  // ---- views ------------------------------------------------------------------------------------------
  const setView = (v) => {
    state.view = v;
    try { localStorage.setItem("hops.view", v); } catch (e) { /* ignore */ }
    if (!location.hash.startsWith(`#${v}`)) history.replaceState(null, "", `#${v}`);
    for (const b of $$(".tab")) b.setAttribute("aria-selected", String(b.dataset.view === v));
    for (const s of $$(".view")) s.hidden = s.id !== `view-${v}`;
    refresh();
  };
  for (const b of $$(".tab")) b.addEventListener("click", () => setView(b.dataset.view));

  // ---- flow map ---------------------------------------------------------------------------------------
  const flowMap = $("#flow-map");
  let mapGeom = null; // service -> {x, y, w, h}

  const renderFlow = (flow) => {
    state.flow = flow;
    const services = flow.services.map((s) => s.service);
    if (!services.length) {
      emptyHint(flowMap);
      mapGeom = null;
      return;
    }
    // columns by kind: router, gateway, backends, others
    const order = { router: 0, gateway: 1, backend: 2, other: 3 };
    const cols = [[], [], [], []];
    for (const s of services) cols[order[kind(s)]].push(s);
    const used = cols.filter((c) => c.length);
    const colW = 250, gapX = 110, rowH = 96, padX = 20, padY = 18;
    const rows = Math.max(...used.map((c) => c.length));
    const W = padX * 2 + used.length * colW + (used.length - 1) * gapX;
    const H = padY * 2 + rows * rowH;
    const root = svg("svg", { viewBox: `0 0 ${W} ${H}`, role: "img", "aria-label": "Services and the hops between them" });
    const defs = svg("defs");
    const marker = svg("marker", { id: "arrow", viewBox: "0 0 10 10", refX: "9", refY: "5", markerWidth: "7", markerHeight: "7", orient: "auto-start-reverse" });
    marker.append(svg("path", { d: "M0,0 L10,5 L0,10 z", fill: "var(--line-2)" }));
    defs.append(marker);
    root.append(defs);
    mapGeom = {};
    const hopsByService = {};
    for (const e of flow.edges) {
      const [svc, name] = e.to.split(":");
      (hopsByService[svc] ||= new Map()).set(name, (hopsByService[svc].get(name) || 0) + e.n);
    }
    const byService = Object.fromEntries(flow.services.map((s) => [s.service, s]));
    used.forEach((col, ci) => {
      const x = padX + ci * (colW + gapX);
      const yOff = (rows - col.length) * rowH / 2;
      col.forEach((s, ri) => {
        const y = padY + yOff + ri * rowH;
        mapGeom[s] = { x, y, w: colW, h: rowH - 18 };
        const g = svg("g", { class: `node ${kind(s)}`, transform: `translate(${x},${y})` });
        g.append(svg("rect", { width: colW, height: rowH - 18 }));
        g.append(svg("text", { x: 12, y: 22, text: s }));
        const st = byService[s];
        g.append(svg("text", { x: 12, y: 40, class: "sub", text: `${st.traces} request${st.traces === 1 ? "" : "s"}${st.errors ? `, ${st.errors} error${st.errors === 1 ? "" : "s"}` : ""}` }));
        // hop pills: the child spans this service ran
        let px = 12;
        const hops = hopsByService[s] ? Array.from(hopsByService[s].entries()).filter(([n]) => n !== "request") : [];
        for (const [name, n] of hops) {
          const w = name.length * 6.6 + 16;
          if (px + w > colW - 8) break;
          const pill = svg("g", { class: "hop-pill", transform: `translate(${px},50)` });
          pill.append(svg("rect", { width: w, height: 18 }));
          pill.append(svg("text", { x: 8, y: 13, text: name }));
          withTip(pill, [[name, `${n} in this window`]]);
          g.append(pill);
          px += w + 6;
        }
        withTip(g, () => [[s, `${st.traces} requests, ${st.errors} errors`]]);
        root.append(g);
      });
    });
    // edges between services (aggregate parent service -> child service, ignoring self edges)
    const agg = new Map();
    for (const e of flow.edges) {
      const a = e.from.split(":")[0], b = e.to.split(":")[0];
      if (a === b) continue;
      const k = `${a}>${b}`;
      const cur = agg.get(k) || { a, b, n: 0, ms: 0 };
      cur.n += e.n; cur.ms += e.avg_ms * e.n;
      agg.set(k, cur);
    }
    const edgesG = svg("g");
    for (const e of agg.values()) {
      const A = mapGeom[e.a], B = mapGeom[e.b];
      if (!A || !B) continue;
      const x1 = A.x + A.w, y1 = A.y + A.h / 2, x2 = B.x, y2 = B.y + B.h / 2;
      const cx = (x1 + x2) / 2;
      const path = svg("path", { class: "edge", d: `M${x1},${y1} C${cx},${y1} ${cx},${y2} ${x2},${y2}`, "marker-end": "url(#arrow)" });
      path.dataset.key = `${e.a}>${e.b}`;
      edgesG.append(path);
      const label = svg("text", { class: "edge-label", x: cx, y: (y1 + y2) / 2 - 6, "text-anchor": "middle", text: `${e.n} · ${fmtMs(e.ms / e.n)}` });
      edgesG.append(label);
    }
    root.prepend(edgesG);
    root.append(svg("g", { id: "pulses" }));
    flowMap.replaceChildren(root);
    $("#flow-legend").replaceChildren(
      ...[["router", "router / harness"], ["gateway", "gateway"], ["backend", "model server, backends"], ["other", "other"]]
        .map(([k, t]) => el("span", { text: t, style: `--c: var(--${k})` })));
    $("#flow-caption").textContent = `${flow.services.length} services and ${agg.size} hops between them in the window; edge labels are count · average time of the hop.`;
  };

  // a request travelling along the map: one pulse per service in start order
  const animateTrace = async (summary) => {
    if (!mapGeom || document.hidden || state.view !== "flow") return;
    let spans;
    try { spans = (await api(`/api/v1/traces/${summary.trace_id}`)).spans; } catch (e) { return; }
    const seq = [];
    for (const s of spans) if (!seq.includes(s.service) && mapGeom[s.service]) seq.push(s.service);
    if (seq.length < 1) return;
    const g = $("#pulses");
    if (!g) return;
    const err = summary.status === "error";
    const dot = svg("circle", { class: `pulse ${kind(seq[0])}${err ? " error" : ""}`, r: 5 });
    g.append(dot);
    const points = seq.map((s) => ({ x: mapGeom[s].x + mapGeom[s].w / 2, y: mapGeom[s].y + mapGeom[s].h / 2 }));
    const total = Math.max(600, Math.min(2200, summary.duration_ms / 4));
    const t0 = performance.now();
    const step = (now) => {
      const p = Math.min(1, (now - t0) / total);
      const segs = Math.max(1, points.length - 1);
      const f = p * segs, i = Math.min(segs - 1, Math.floor(f)), u = f - i;
      const a = points[i], b = points[Math.min(points.length - 1, i + 1)];
      const ease = u < .5 ? 2 * u * u : 1 - Math.pow(-2 * u + 2, 2) / 2;
      dot.setAttribute("cx", a.x + (b.x - a.x) * ease);
      dot.setAttribute("cy", a.y + (b.y - a.y) * ease - Math.sin(u * Math.PI) * 18);
      dot.setAttribute("class", `pulse ${kind(seq[Math.min(seq.length - 1, Math.round(f))])}${err ? " error" : ""}`);
      if (p < 1) requestAnimationFrame(step); else setTimeout(() => dot.remove(), 250);
    };
    requestAnimationFrame(step);
  };

  const ticker = $("#ticker");
  const chipFor = (t, fresh) => {
    const c = el("li", { class: `chip${t.status === "error" ? " error" : ""}${fresh ? " new" : ""}`, role: "button", tabindex: "0",
      onclick: () => openTrace(t.trace_id), onkeydown: (e) => { if (e.key === "Enter") openTrace(t.trace_id); } },
      el("span", { class: "sw", style: `--c: ${colorOf(t.service)}` }),
      el("span", { text: `${t.attrs.client || t.service} → ${modelOf(t)}` }),
      el("span", { class: "ms", text: fmtMs(t.duration_ms) }));
    if (t.attrs.error) c.append(el("span", { class: "pill bad", text: String(t.attrs.error) }));
    return c;
  };
  const renderTicker = (traces) => ticker.replaceChildren(...traces.slice(0, 10).map((t) => chipFor(t, false)));
  const pushTicker = (t) => { ticker.prepend(chipFor(t, true)); while (ticker.children.length > 10) ticker.lastChild.remove(); };

  // ---- kpis and stats ---------------------------------------------------------------------------------
  const kpi = (v, l, cls = "") => el("div", { class: `kpi ${cls}` }, el("div", { class: "v", text: v }), el("div", { class: "l", text: l }));
  const emptyHint = async (target) => {
    let newest = null;
    try { newest = (await api("/api/v1/traces", { limit: 1 })).traces[0]; } catch (e) { /* the caption below covers it */ }
    const wider = [...$("#window").options].map((o) => Number(o.value)).find((v) => v > state.windowMs);
    target.replaceChildren(el("p", { class: "muted" },
      newest ? `Nothing in this window. The newest trace is from ${fmtDate(newest.start_ms)}. ` : "No traces yet. ",
      wider && newest ? el("button", { class: "btn", text: `Show the last ${$("#window").querySelector(`option[value="${wider}"]`).textContent.replace("last ", "")}`,
        onclick: () => { $("#window").value = String(wider); state.windowMs = wider; refresh(); } }) : null,
      newest ? null : el("span", {}, "Send some: ", el("code", { text: "llm-hops demo" }), ", or follow a request log with ", el("code", { text: "llm-hops serve -tail FILE" }), ".")));
  };
  const renderKpis = (target, st) => {
    const errPct = st.requests ? (100 * st.errors / st.requests) : 0;
    const perMin = st.requests / Math.max(1, (st.until_ms - st.since_ms) / 60000);
    target.replaceChildren(
      kpi(String(st.requests), "requests in the window"),
      kpi(perMin < 10 ? perMin.toFixed(2) : perMin.toFixed(0), "requests per minute"),
      kpi(`${errPct.toFixed(errPct < 10 ? 1 : 0)} %`, `errors (${st.errors})`, errPct > 5 ? "bad" : errPct > 1 ? "warn" : ""),
      kpi(fmtMs(st.p50_ms), "median request"),
      kpi(fmtMs(st.p95_ms), "95th percentile", st.p95_ms > 30000 ? "warn" : ""),
      kpi(String(st.model_switches), "model switches", st.model_switches > 20 ? "warn" : ""),
      kpi(st.loads ? `${st.loads} · ${fmtMs(st.load_ms)}` : "0", "model loads · time loading", st.load_ms > 600000 ? "warn" : ""));
  };

  const renderSplit = (st) => {
    const bar = $("#split"), body = $("#backends tbody");
    const total = st.requests || 0;
    const byKind = { local: 0, cloud: 0 };
    for (const b of st.backends || []) byKind[b.backend === "cloud" ? "cloud" : "local"] += b.requests;
    bar.className = "split" + (total ? "" : " empty");
    bar.replaceChildren(...["local", "cloud"].filter((k) => byKind[k]).map((k) => {
      const pct = Math.round(100 * byKind[k] / total);
      return withTip(el("div", { class: k, style: `flex: ${byKind[k]} 0 0`, text: pct >= 8 ? `${k} ${pct} %` : "" }), [[k, `${byKind[k]} requests, ${pct} %`]]);
    }));
    body.replaceChildren(...(st.backends || []).map((b) => el("tr", {}, el("td", {}, el("span", { class: "sw", style: `--c: var(--${b.backend === "cloud" ? "backend" : "gateway"})` }), b.backend),
      el("td", { text: b.provider }), el("td", { class: "num", text: String(b.requests) }), el("td", { class: "num", text: String(b.errors) }),
      el("td", { class: "num", text: b.prompt_tokens.toLocaleString() }), el("td", { class: "num", text: b.completion_tokens.toLocaleString() }),
      el("td", { class: "num", text: fmtMs(b.p50_ms) }), el("td", { class: "num", text: fmtMs(b.p95_ms) }))));
    const cloudTok = (st.backends || []).filter((b) => b.backend === "cloud").reduce((a, b) => a + b.prompt_tokens + b.completion_tokens, 0);
    $("#split-caption").textContent = total ? `${byKind.local} local and ${byKind.cloud} cloud requests in the window; ${cloudTok.toLocaleString()} tokens went to the cloud.` : "Nothing in this window.";
  };
  const renderRpm = (st) => {
    const target = $("#rpm-chart");
    const buckets = st.buckets;
    if (!buckets.length) { emptyHint(target); return; }
    const W = 900, H = 180, L = 40, R = 10, T = 10, B = 26;
    const t0 = st.since_ms, t1 = st.until_ms, bw = st.bucket_ms;
    const n = Math.max(1, Math.ceil((t1 - t0) / bw));
    const counts = new Map(buckets.map((b) => [b.t, b.n]));
    const maxN = Math.max(1, ...buckets.map((b) => b.n));
    const root = svg("svg", { viewBox: `0 0 ${W} ${H}`, role: "img", "aria-label": "Requests per bucket over the window" });
    const x = (t) => L + (t - t0) / (t1 - t0) * (W - L - R);
    const y = (v) => T + (1 - v / maxN) * (H - T - B);
    for (const f of [0, .5, 1]) {
      root.append(svg("line", { class: "grid", x1: L, x2: W - R, y1: y(maxN * f), y2: y(maxN * f) }));
      root.append(svg("text", { class: "axis", x: L - 6, y: y(maxN * f) + 4, "text-anchor": "end", text: String(Math.round(maxN * f)) }));
    }
    const barW = Math.max(1, (W - L - R) / n - 1);
    for (let i = 0; i < n; i++) {
      const bt = Math.floor(t0 / bw) * bw + i * bw;
      const c = counts.get(bt) || 0;
      if (!c) continue;
      const g = svg("g");
      const r = svg("rect", { class: "bar-ok", x: x(bt), y: y(c), width: barW, height: y(0) - y(c), rx: 1.5 });
      withTip(r, [[`${c} request${c === 1 ? "" : "s"}`, `${fmtTime(bt)} to ${fmtTime(bt + bw)}`]]);
      g.append(r);
      root.append(g);
    }
    for (const f of [0, .25, .5, .75, 1]) {
      const t = t0 + (t1 - t0) * f;
      root.append(svg("text", { class: "axis", x: x(t), y: H - 8, "text-anchor": f === 0 ? "start" : f === 1 ? "end" : "middle", text: fmtTime(t) }));
    }
    target.replaceChildren(root);
    const bucketText = bw >= 3600000 ? `${Math.round(bw / 3600000)} h` : `${Math.round(bw / 60000)} min`;
    $("#rpm-title").textContent = `Requests per ${bucketText === "1 min" ? "minute" : bucketText}`;
    $("#rpm-caption").textContent = `${st.requests} requests in the window, ${Math.round((t1 - t0) / 3600000)} hours; one bar per ${bucketText}.`;
  };

  const renderHopTable = (st) => {
    const body = $("#hops tbody");
    const names = Object.keys(st.hops);
    const totalP50 = names.reduce((a, k) => a + st.hops[k].p50_ms * st.hops[k].n, 0) || 1;
    body.replaceChildren(...names.sort((a, b) => st.hops[b].p50_ms * st.hops[b].n - st.hops[a].p50_ms * st.hops[a].n).map((k) => {
      const h = st.hops[k];
      const share = h.p50_ms * h.n / totalP50;
      return el("tr", {}, el("td", { text: k }), el("td", { class: "num", text: String(h.n) }), el("td", { class: "num", text: fmtMs(h.p50_ms) }),
        el("td", { class: "num", text: fmtMs(h.p95_ms) }),
        el("td", {}, el("span", { class: "bar", style: `width:${Math.max(2, Math.round(share * 120))}px` }), " ", el("span", { class: "muted", text: `${Math.round(share * 100)} %` })));
    }));
  };
  const renderModelTable = (st) => {
    $("#models tbody").replaceChildren(...st.models.map((m) => el("tr", {}, el("td", { text: m.model }), el("td", { class: "num", text: String(m.requests) }),
      el("td", { class: "num", text: String(m.errors) }), el("td", { class: "num", text: fmtMs(m.p50_ms) }), el("td", { class: "num", text: fmtMs(m.p95_ms) }),
      el("td", { class: "num", text: m.tokens_out.toLocaleString() }))));
  };

  // ---- the model timeline -----------------------------------------------------------------------------
  const MODEL_COLORS = ["#2a78d6", "#d97757", "#6b5bd2", "#2f8f5b", "#c9821b", "#7a8794", "#c8412f", "#3c8f7a"];
  const modelColor = (() => { const seen = new Map(); return (m) => { if (!seen.has(m)) seen.set(m, MODEL_COLORS[seen.size % MODEL_COLORS.length]); return seen.get(m); }; })();
  const renderTimeline = (data) => {
    const target = $("#timeline"), legend = $("#timeline-legend");
    const t0 = data.since_ms, t1 = Math.min(data.until_ms, Date.now());
    const loads = data.loads || [];
    if (!loads.length) { target.replaceChildren(el("p", { class: "muted", text: "No model loads in this window (the model server's log is not followed, or nothing was loaded)." })); legend.replaceChildren(); return; }
    const W = 1200, H = 62, L = 8, R = 8, T = 6, bandY = 14, bandH = 22;
    const x = (t) => L + Math.max(0, Math.min(1, (t - t0) / (t1 - t0))) * (W - L - R);
    const root = svg("svg", { viewBox: `0 0 ${W} ${H}`, role: "img", "aria-label": "Which model was loaded when" });
    const models = new Set();
    let switches = 0;
    loads.forEach((l, i) => {
      const next = loads[i + 1];
      const from = Math.max(l.end_ms, t0), to = next ? Math.max(next.start_ms, from) : t1;
      models.add(l.model);
      if (i > 0 && loads[i - 1].model !== l.model) switches++;
      if (to > from) {
        const g = svg("g");
        const band = svg("rect", { class: "band", x: x(from), y: bandY, width: Math.max(1, x(to) - x(from)), height: bandH, fill: modelColor(l.model), rx: 3 });
        withTip(band, [[l.model, `loaded ${fmtTime(l.start_ms)} in ${fmtMs(l.end_ms - l.start_ms)}, in the GPU ${fmtMs(to - from)}`]]);
        g.append(band);
        if (x(to) - x(from) > l.model.length * 6.5 + 10) g.append(svg("text", { class: "band-label", x: x(from) + 6, y: bandY + 15, text: l.model }));
        root.append(g);
      }
      if (l.start_ms >= t0) {
        const m = svg("rect", { class: "loadmark", x: x(l.start_ms), y: bandY - 4, width: Math.max(1.5, x(l.end_ms) - x(l.start_ms)), height: bandH + 8, rx: 1 });
        withTip(m, [[`load ${l.model}`, `${fmtTime(l.start_ms)}, ${fmtMs(l.end_ms - l.start_ms)}`]]);
        root.append(m);
      }
    });
    for (const f of [0, .25, .5, .75, 1]) {
      const t = t0 + (t1 - t0) * f;
      root.append(svg("text", { class: "axis", x: x(t), y: H - 6, "text-anchor": f === 0 ? "start" : f === 1 ? "end" : "middle", text: fmtTime(t) }));
    }
    target.replaceChildren(root);
    legend.replaceChildren(...[...models].map((m) => el("span", { text: m, style: `--c: ${modelColor(m)}` })));
    const inWindow = loads.filter((l) => l.start_ms >= t0).length;
    $("#timeline-caption").textContent = `${inWindow} load${inWindow === 1 ? "" : "s"} and ${switches} switch${switches === 1 ? "" : "es"} in the window; a dark mark is a load, the band after it is the model in the GPU until the next load.`;
  };

  // ---- traces table -----------------------------------------------------------------------------------
  const tbody = $("#traces tbody");
  const hopBar = (t) => {
    const total = Math.max(1, t.duration_ms);
    const q = Number(t.attrs.queue_ms || 0), ttft = Number(t.attrs.ttft_ms || 0);
    const parts = [];
    if (q) parts.push(["gateway", q]);
    if (ttft) parts.push(["backend", ttft]);
    parts.push(["other", Math.max(0, total - q - ttft)]);
    const wrap = el("span", { class: "hops" });
    for (const [k, v] of parts) wrap.append(el("i", { style: `--c: var(--${k}); width:${Math.max(3, Math.round(v / total * 90))}px`, title: `${k} ${fmtMs(v)}` }));
    return wrap;
  };
  const rowFor = (t, fresh) => {
    const status = Number(t.attrs.status_code || 0);
    const tr = el("tr", { "data-id": t.trace_id, class: fresh ? "new" : "", tabindex: "0", onclick: () => openTrace(t.trace_id),
      onkeydown: (e) => { if (e.key === "Enter") openTrace(t.trace_id); } },
      el("td", { text: fmtTime(t.start_ms), title: fmtDate(t.start_ms) }),
      el("td", { text: t.attrs.client || "" }),
      el("td", { text: t.attrs.wire || t.name }),
      el("td", {}, el("span", { class: "sw", style: `--c: ${colorOf(t.service)}` }), t.attrs.role ? `${t.attrs.role} → ` : "", modelOf(t)),
      el("td", {}, el("span", { class: `pill ${t.status === "error" ? "bad" : "ok"}`, text: t.attrs.error ? String(t.attrs.error) : status ? String(status) : t.status })),
      el("td", { class: "num", text: t.attrs.queue_ms ? fmtMs(Number(t.attrs.queue_ms)) : "" }),
      el("td", { class: "num", text: t.attrs.ttft_ms ? fmtMs(Number(t.attrs.ttft_ms)) : "" }),
      el("td", { class: "num", text: fmtMs(t.duration_ms) }),
      el("td", { class: "num", text: t.attrs.prompt_tokens != null ? `${Number(t.attrs.prompt_tokens).toLocaleString()}${t.attrs.completion_tokens != null ? ` / ${Number(t.attrs.completion_tokens).toLocaleString()}` : ""}` : "" }),
      el("td", { class: "num", text: t.attrs.tok_per_s != null ? Number(t.attrs.tok_per_s).toFixed(1) : "" }),
      el("td", {}, hopBar(t)));
    if (state.selected === t.trace_id) tr.classList.add("sel");
    return tr;
  };
  const renderTraces = (traces) => {
    state.traces = traces;
    tbody.replaceChildren(...traces.map((t) => rowFor(t, false)));
    $("#traces-empty").hidden = traces.length > 0;
    if (!traces.length) emptyHint($("#traces-empty"));
    $("#traces-count").textContent = traces.length ? `${traces.length} trace${traces.length === 1 ? "" : "s"}${traces.length >= 500 ? " (the newest 500)" : ""}` : "";
  };
  const matchesFilters = (t) => {
    const f = state.filters;
    if (f.model && modelOf(t) !== f.model) return false;
    if (f.client && (t.attrs.client || "") !== f.client) return false;
    if (f.service && t.service !== f.service) return false;
    if (f.status && t.status !== f.status) return false;
    if (f.min && t.duration_ms < Number(f.min)) return false;
    if (state.q && !JSON.stringify(t).toLowerCase().includes(state.q.toLowerCase())) return false;
    return true;
  };
  const upsertTrace = (t) => {
    const i = state.traces.findIndex((x) => x.trace_id === t.trace_id);
    const fresh = i < 0;
    if (fresh) state.traces.unshift(t); else state.traces[i] = t;
    if (!matchesFilters(t)) return;
    const existing = tbody.querySelector(`tr[data-id="${CSS.escape(t.trace_id)}"]`);
    const row = rowFor(t, fresh);
    if (existing) existing.replaceWith(row); else tbody.prepend(row);
    while (tbody.children.length > 500) tbody.lastChild.remove();
    $("#traces-empty").hidden = true;
  };

  const renderFacets = (f) => {
    const fill = (id, vals, cur) => {
      const s = $(id);
      s.replaceChildren(el("option", { value: "", text: "all" }), ...vals.map((v) => el("option", { value: v, text: v })));
      s.value = vals.includes(cur) ? cur : "";
    };
    fill("#f-model", f.model || [], state.filters.model);
    fill("#f-client", f.client || [], state.filters.client);
    fill("#f-service", f.service || [], state.filters.service);
  };

  // ---- trace detail -----------------------------------------------------------------------------------
  const drawer = $("#drawer");
  const openTrace = async (id) => {
    let data;
    try { data = await api(`/api/v1/traces/${id}`); } catch (e) { return; }
    state.selected = id;
    for (const r of $$("#traces tbody tr")) r.classList.toggle("sel", r.dataset.id === id);
    const t = data.trace, spans = data.spans;
    $("#d-title").textContent = `${t.attrs.client ? `${t.attrs.client} → ` : ""}${modelOf(t)}${t.attrs.wire ? ` (${t.attrs.wire})` : ""}`;
    $("#d-sub").textContent = `${fmtDate(t.start_ms)} · ${fmtMs(t.duration_ms)} · ${spans.length} span${spans.length === 1 ? "" : "s"} · ${id}`;
    $("#d-copy").onclick = () => { navigator.clipboard?.writeText(id); $("#d-copy").textContent = "Copied"; setTimeout(() => { $("#d-copy").textContent = "Copy id"; }, 1200); };
    renderWaterfall(t, spans);
    renderSpanList(spans);
    drawer.hidden = false;
    history.replaceState(null, "", `#${state.view}&trace=${id}`);
    $("#d-close").focus();
  };
  const closeDrawer = () => { drawer.hidden = true; state.selected = null; history.replaceState(null, "", `#${state.view}`); for (const r of $$("#traces tbody tr.sel")) r.classList.remove("sel"); };
  $("#d-close").addEventListener("click", closeDrawer);

  const renderWaterfall = (t, spans) => {
    // depth by parent chain, order by start
    const byId = Object.fromEntries(spans.map((s) => [s.span_id, s]));
    const depth = (s) => { let d = 0, p = s.parent_id; while (p && byId[p] && d < 20) { d++; p = byId[p].parent_id; } return d; };
    const ordered = spans.slice().sort((a, b) => a.start_ms - b.start_ms || (b.end_ms - b.start_ms) - (a.end_ms - a.start_ms));
    const t0 = Math.min(...spans.map((s) => s.start_ms)), t1 = Math.max(...spans.map((s) => s.end_ms), t0 + 1);
    const W = 740, L = 230, R = 70, rowH = 26, T = 22;
    const H = T + ordered.length * rowH + 8;
    const root = svg("svg", { viewBox: `0 0 ${W} ${H}`, role: "img", "aria-label": "Waterfall of the spans" });
    const x = (ms) => L + (ms - t0) / (t1 - t0) * (W - L - R);
    const nTicks = 5;
    for (let i = 0; i <= nTicks; i++) {
      const ms = t0 + (t1 - t0) * i / nTicks;
      root.append(svg("line", { class: "tick", x1: x(ms), x2: x(ms), y1: T - 4, y2: H }));
      root.append(svg("text", { class: "tick-label", x: x(ms), y: T - 8, "text-anchor": i === nTicks ? "end" : "middle", text: fmtMs(ms - t0) }));
    }
    ordered.forEach((s, i) => {
      const y = T + i * rowH;
      const d = depth(s);
      const g = svg("g");
      const lane = svg("rect", { class: "lane", x: 0, y, width: W, height: rowH });
      g.append(lane);
      g.append(svg("text", { class: "label svc", x: 8 + d * 12, y: y + 17, text: s.service }));
      g.append(svg("text", { class: "label", x: 8 + d * 12 + s.service.length * 6.8 + 8, y: y + 17, text: s.name }));
      const x1 = x(s.start_ms), x2 = Math.max(x1 + 2, x(s.end_ms));
      g.append(svg("rect", { class: `bar ${kind(s.service)}${s.status === "error" ? " error" : ""}`, x: x1, y: y + 6, width: x2 - x1, height: rowH - 12, rx: 3 }));
      if (s.attrs.ttft_ms != null) {
        const mx = x(s.start_ms + Number(s.attrs.ttft_ms));
        g.append(svg("line", { class: "marker", x1: mx, x2: mx, y1: y + 3, y2: y + rowH - 3 }));
      }
      g.append(svg("text", { class: "dur", x: Math.min(W - R + 4, x2 + 6), y: y + 17, text: fmtMs(s.end_ms - s.start_ms) }));
      const lines = [[`${s.service} ${s.name}`, fmtMs(s.end_ms - s.start_ms)], `starts at +${fmtMs(s.start_ms - t0)}`];
      for (const [k, v] of Object.entries(s.attrs)) if (v !== null && v !== "") lines.push([`${k}:`, String(v)]);
      withTip(g, lines);
      root.append(g);
    });
    $("#d-waterfall").replaceChildren(root);
  };
  const renderSpanList = (spans) => {
    $("#d-spans").replaceChildren(...spans.map((s) => {
      const dl = el("dl");
      for (const [k, v] of Object.entries(s.attrs)) { dl.append(el("dt", { text: k }), el("dd", { text: String(v) })); }
      dl.append(el("dt", { text: "span" }), el("dd", { text: `${s.span_id}${s.parent_id ? ` ← ${s.parent_id}` : ""}` }));
      return el("details", {}, el("summary", {}, el("span", { class: "sw", style: `--c: ${colorOf(s.service)}` }),
        el("b", { text: `${s.service} ${s.name}` }), el("span", { class: "muted", text: fmtMs(s.end_ms - s.start_ms) }),
        s.status === "error" ? el("span", { class: "pill bad", text: "error" }) : null), dl);
    }));
  };

  // ---- data refresh -----------------------------------------------------------------------------------
  const refresh = async () => {
    const w = windowParams();
    try {
      if (state.view === "flow") {
        const [flow, st, traces] = await Promise.all([api("/api/v1/flow", w), api("/api/v1/stats", { ...w, bucket_ms: bucketFor() }),
          api("/api/v1/traces", { since: w.since, limit: 10 })]);
        renderFlow(flow); renderKpis($("#flow-kpis"), st); renderSplit(st); renderTicker(traces.traces);
      } else if (state.view === "traces") {
        const [traces, facets, loads] = await Promise.all([api("/api/v1/traces", { since: w.since, limit: 500, model: state.filters.model, client: state.filters.client,
          service: state.filters.service, status: state.filters.status, min_ms: state.filters.min, q: state.q }), api("/api/v1/facets", w), api("/api/v1/loads", w)]);
        renderFacets(facets); renderTraces(traces.traces); renderTimeline(loads);
      } else {
        const st = await api("/api/v1/stats", { ...w, bucket_ms: bucketFor() });
        state.stats = st; renderKpis($("#stats-kpis"), st); renderRpm(st); renderHopTable(st); renderModelTable(st);
      }
    } catch (e) {
      $("#live-text").textContent = String(e.message || "").startsWith("401") ? "token required: open the page as /#token=..." : "server unreachable";
      $("#live").className = "live";
    }
  };
  const bucketFor = () => state.windowMs <= 900000 ? 30000 : state.windowMs <= 3600000 ? 60000 : state.windowMs <= 21600000 ? 300000 : state.windowMs <= 86400000 ? 900000 : 3600000;

  // ---- live stream ------------------------------------------------------------------------------------
  const connect = () => {
    const es = new EventSource("/api/v1/stream");
    state.es = es;
    es.onopen = () => { $("#live").className = state.paused ? "live paused" : "live on"; $("#live-text").textContent = state.paused ? "paused" : "live"; };
    es.onerror = () => { $("#live").className = "live"; $("#live-text").textContent = "reconnecting"; };
    es.onmessage = (m) => {
      if (state.paused) return;
      let ev;
      try { ev = JSON.parse(m.data); } catch (e) { return; }
      if (ev.type !== "trace") return;
      const t = ev.trace;
      if (state.view === "flow") { pushTicker(t); animateTrace(t); scheduleRefresh(); }
      else if (state.view === "traces") upsertTrace(t);
      else scheduleRefresh();
    };
  };
  let refreshQueued = false;
  const scheduleRefresh = () => { if (refreshQueued) return; refreshQueued = true; setTimeout(() => { refreshQueued = false; refresh(); }, 2500); };

  // ---- controls ---------------------------------------------------------------------------------------
  $("#window").value = String(state.windowMs);
  $("#window").addEventListener("change", (e) => { state.windowMs = Number(e.target.value); refresh(); });
  let qTimer;
  $("#q").addEventListener("input", (e) => { state.q = e.target.value.trim(); clearTimeout(qTimer); qTimer = setTimeout(() => { if (state.view !== "traces") setView("traces"); else refresh(); }, 250); });
  const pauseBtn = $("#pause");
  const setPaused = (p) => { state.paused = p; pauseBtn.setAttribute("aria-pressed", String(p)); pauseBtn.textContent = p ? "Resume" : "Pause";
    $("#live").className = p ? "live paused" : "live on"; $("#live-text").textContent = p ? "paused" : "live"; if (!p) refresh(); };
  pauseBtn.addEventListener("click", () => setPaused(!state.paused));
  $("#theme").addEventListener("click", () => {
    const next = state.theme === null ? "light" : state.theme === "light" ? "dark" : null;
    state.theme = next;
    if (next) document.documentElement.dataset.theme = next; else delete document.documentElement.dataset.theme;
    try { if (next) localStorage.setItem("hops.theme", next); else localStorage.removeItem("hops.theme"); } catch (e) { /* ignore */ }
  });
  for (const [id, key] of [["#f-model", "model"], ["#f-client", "client"], ["#f-service", "service"], ["#f-status", "status"], ["#f-min", "min"]]) {
    $(id).addEventListener("change", (e) => { state.filters[key] = e.target.value; refresh(); });
  }
  document.addEventListener("keydown", (e) => {
    if (e.target.matches("input, select, textarea")) return;
    if (e.key === "Escape") closeDrawer();
    if (e.key === " ") { e.preventDefault(); setPaused(!state.paused); }
    if (e.key === "/") { e.preventDefault(); $("#q").focus(); }
    if (e.key === "1") setView("flow"); if (e.key === "2") setView("traces"); if (e.key === "3") setView("stats");
    if ((e.key === "j" || e.key === "k") && state.view === "traces") {
      const rows = $$("#traces tbody tr"); if (!rows.length) return;
      const i = rows.findIndex((r) => r.dataset.id === state.selected);
      const next = rows[Math.max(0, Math.min(rows.length - 1, i + (e.key === "j" ? 1 : -1)))];
      if (next) { openTrace(next.dataset.id); next.scrollIntoView({ block: "nearest" }); }
    }
  });
  document.addEventListener("visibilitychange", () => { if (!document.hidden) refresh(); });

  session.then(() => {
    setView(state.view);
    if (hashTrace) openTrace(hashTrace);
    connect();
  });
  setInterval(() => { if (!state.paused && !document.hidden) refresh(); }, 30000);
})();
