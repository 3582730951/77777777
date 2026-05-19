// charts.js — minimal inline SVG renderers, no external deps.
// Designed to fit Apple HIG aesthetic: thin strokes, smooth curves, subtle fills.

(function () {
  const ns = "http://www.w3.org/2000/svg";

  function makeSvg(w, h, viewBox, opts) {
    const s = document.createElementNS(ns, "svg");
    s.setAttribute("width", opts?.pxSize ? w : "100%");
    s.setAttribute("height", opts?.pxSize ? h : "100%");
    s.setAttribute("viewBox", viewBox || `0 0 ${w} ${h}`);
    s.setAttribute("preserveAspectRatio", opts?.stretch ? "none" : "xMidYMid meet");
    return s;
  }

  function el(name, attrs) {
    const e = document.createElementNS(ns, name);
    if (attrs) for (const [k, v] of Object.entries(attrs)) e.setAttribute(k, v);
    return e;
  }

  // fmtTokens: smart token unit display.
  // <1M → "123,456" (raw), ≥1M → "1.23M", ≥1T → "1.23T", ≥1P → "1.23P"
  window.fmtTokens = function(n) {
    if (n == null || isNaN(n)) return '0';
    n = Math.abs(n);
    if (n >= 1e15) return (n / 1e15).toFixed(2).replace(/\.?0+$/, '') + 'P';
    if (n >= 1e12) return (n / 1e12).toFixed(2).replace(/\.?0+$/, '') + 'T';
    if (n >= 1e9)  return (n / 1e9).toFixed(2).replace(/\.?0+$/, '') + 'G';
    if (n >= 1e6)  return (n / 1e6).toFixed(2).replace(/\.?0+$/, '') + 'M';
    if (n >= 1e3)  return (n / 1e3).toFixed(1).replace(/\.?0+$/, '') + 'K';
    return n.toLocaleString();
  };

  function fmtCompact(n) {
    if (n >= 1e6) return (n / 1e6).toFixed(1).replace(/\.0$/, '') + 'M';
    if (n >= 1e4) return (n / 1e3).toFixed(1).replace(/\.0$/, '') + 'K';
    if (n >= 1e3) return (n / 1e3).toFixed(1).replace(/\.0$/, '') + 'K';
    return String(Math.round(n));
  }

  window.renderRequestSeries = function (host, rollups) {
    if (!rollups || !Array.isArray(rollups) || rollups.length === 0) return;
    host.innerHTML = "";
    const configuredH = parseInt(host.dataset.chartHeight || "", 10) || 0;
    const oldH = host.clientHeight || host.offsetHeight || 220;
    host.style.height = "auto";
    const W = host.clientWidth || host.offsetWidth || 800;
    const H = configuredH || Math.max(150, Math.min(220, oldH));
    host.style.minHeight = (H + 8) + "px";
    const pad = { l: 44, r: 10, t: 14, b: 24 };
    const innerW = W - pad.l - pad.r;
    const innerH = H - pad.t - pad.b;

    const counts = rollups.map(r => r.Count || 0);
    const lats = rollups.map(r => r.AvgLatencyMs || 0);
    const errs = rollups.map(r => r.ErrCount || 0);
    const totalCount = counts.reduce((a, b) => a + b, 0);

    const maxCnt = Math.max(1, ...counts);
    const maxLat = Math.max(1, ...lats);
    const N = rollups.length;

    const svg = makeSvg(W, H, null, {pxSize: true});
    svg.style.maxWidth = "100%";
    svg.style.height = H + "px";
    host.appendChild(svg);

    if (totalCount === 0) {
      svg.appendChild(el("rect", {
        x: pad.l, y: pad.t, width: innerW, height: innerH,
        rx: 10, fill: "rgba(127,127,127,0.035)", stroke: "rgba(127,127,127,0.12)",
      }));
      const bars = [0.26, 0.42, 0.34, 0.58, 0.48, 0.36, 0.62, 0.52, 0.39, 0.46, 0.31, 0.55];
      const barGap = 7;
      const barW = Math.max(6, (innerW - barGap * (bars.length + 1)) / bars.length);
      bars.forEach((v, i) => {
        const h = Math.max(10, innerH * v * 0.52);
        const x = pad.l + barGap + i * (barW + barGap);
        const y = pad.t + innerH - h - 12;
        svg.appendChild(el("rect", {
          x, y, width: barW, height: h, rx: 5,
          fill: "var(--accent)", opacity: String(0.06 + (i % 3) * 0.025),
        }));
      });
      svg.appendChild(el("text", {
        x: W / 2, y: pad.t + innerH / 2 - 8, "text-anchor": "middle",
        "font-size": "13", "font-weight": "650", fill: "currentColor", opacity: "0.72",
      })).textContent = "暂无请求流量";
      svg.appendChild(el("text", {
        x: W / 2, y: pad.t + innerH / 2 + 14, "text-anchor": "middle",
        "font-size": "11", fill: "currentColor", opacity: "0.48",
      })).textContent = "请求进入后这里展示吞吐、延迟与错误趋势";
      return;
    }

    // Grid
    for (let i = 0; i <= 4; i++) {
      const y = pad.t + (innerH * i) / 4;
      svg.appendChild(el("line", {
        x1: pad.l, x2: W - pad.r, y1: y, y2: y,
        stroke: "rgba(127,127,127,0.15)", "stroke-width": "1",
      }));
      svg.appendChild(el("text", {
        x: pad.l - 6, y: y + 4, "text-anchor": "end",
        "font-size": "10", fill: "currentColor", opacity: "0.5",
      })).textContent = fmtCompact(maxCnt - (maxCnt * i) / 4);
    }

    // Request count area
    const stepX = innerW / Math.max(1, N - 1);
    let pathD = "";
    for (let i = 0; i < N; i++) {
      const x = pad.l + i * stepX;
      const y = pad.t + innerH - (counts[i] / maxCnt) * innerH;
      pathD += (i === 0 ? "M" : "L") + x + "," + y + " ";
    }
    const fillPath = pathD + `L${pad.l + (N - 1) * stepX},${pad.t + innerH} L${pad.l},${pad.t + innerH} Z`;

    if (totalCount > 0) {
      svg.appendChild(el("path", {
        d: fillPath, fill: "url(#grad-blue)", opacity: "0.18",
      }));
      svg.appendChild(el("path", {
        d: pathD, fill: "none", stroke: "var(--viz-accent,var(--accent))",
        "stroke-width": "1.6", "stroke-linejoin": "round", "stroke-linecap": "round",
        opacity: "0.82",
      }));
    }

    // Error bars at bottom
    for (let i = 0; i < N; i++) {
      if (errs[i] === 0) continue;
      const x = pad.l + i * stepX - 1.5;
      const h = (errs[i] / maxCnt) * innerH;
      svg.appendChild(el("rect", {
        x, y: pad.t + innerH - h, width: 3, height: h, fill: "var(--err)", opacity: 0.7,
      }));
    }

    // Defs (gradient)
    const defs = el("defs");
    const grad = el("linearGradient", { id: "grad-blue", x1: "0", x2: "0", y1: "0", y2: "1" });
    grad.appendChild(el("stop", { offset: "0%", "stop-color": "var(--viz-accent,var(--accent))", "stop-opacity": "0.42" }));
    grad.appendChild(el("stop", { offset: "100%", "stop-color": "var(--viz-accent,var(--accent))", "stop-opacity": "0.03" }));
    defs.appendChild(grad);
    svg.appendChild(defs);

    // X axis ticks (3 labels)
    const fmt = (ts) => {
      const d = new Date(ts);
      return d.getHours().toString().padStart(2, "0") + ":" + d.getMinutes().toString().padStart(2, "0");
    };
    [0, Math.floor(N / 2), N - 1].forEach((i) => {
      const x = pad.l + i * stepX;
      const t = el("text", {
        x, y: H - 8, "text-anchor": "middle",
        "font-size": "10", fill: "currentColor", opacity: "0.55",
      });
      t.textContent = fmt(rollups[i].Bucket);
      svg.appendChild(t);
    });

    // Total caption
    const total = counts.reduce((a, b) => a + b, 0);
    const caption = document.createElement("div");
    caption.className = "chart-caption";
    caption.innerHTML = `共 <b>${total}</b> 次请求 · 平均延迟 <b>${Math.round(lats.reduce((a, b) => a + b, 0) / Math.max(1, lats.filter(x => x).length) || 0)} ms</b> · 错误 <b>${errs.reduce((a, b) => a + b, 0)}</b>`;
    host.appendChild(caption);
  };

  window.renderConfidenceDonut = function (host, buckets) {
    if (!buckets || !Array.isArray(buckets)) return;
    host.innerHTML = "";
    host.style.height = "auto";
    const W = host.clientWidth || host.offsetWidth || 320;
    const configuredH = Number(host.dataset.chartHeight || 0);
    const H = configuredH > 0 ? configuredH : 132;
    const cx = W / 2, cy = H / 2, r = Math.max(34, Math.min(W, H) / 2 - 10), ir = Math.max(18, r - 18);

    const colors = {
      healthy: "var(--viz-ok,var(--ok))",
      low_quota: "var(--warn)",
      no_quota: "var(--err)",
      banned: "var(--err)",
      abnormal: "var(--fg-muted)",
    };
    const total = buckets.reduce((a, b) => a + (b.count || 0), 0);

    const svg = makeSvg(W, H, null, {pxSize: true});
    svg.style.maxWidth = "100%";
    svg.style.height = H + "px";
    host.appendChild(svg);

    let start = -Math.PI / 2;
    if (total === 0) {
      svg.appendChild(el("circle", { cx, cy, r, fill: "none", stroke: "var(--hairline)", "stroke-width": "22" }));
    } else {
      for (const b of buckets) {
        if (!b.count) continue;
        const frac = b.count / total;
        const key = b.status_category || b.confidence;
        const color = colors[key] || "#999";
        // Full circle: SVG arc can't draw start==end, use two semicircles
        if (frac >= 0.999) {
          const midR = (r + ir) / 2;
          const sw = r - ir;
          svg.appendChild(el("circle", { cx, cy, r: midR, fill: "none", stroke: color, "stroke-width": sw }));
        } else {
          const end = start + frac * Math.PI * 2;
          const large = frac > 0.5 ? 1 : 0;
          const x1 = cx + Math.cos(start) * r, y1 = cy + Math.sin(start) * r;
          const x2 = cx + Math.cos(end) * r, y2 = cy + Math.sin(end) * r;
          const ix1 = cx + Math.cos(end) * ir, iy1 = cy + Math.sin(end) * ir;
          const ix2 = cx + Math.cos(start) * ir, iy2 = cy + Math.sin(start) * ir;
          const d = `M ${x1} ${y1} A ${r} ${r} 0 ${large} 1 ${x2} ${y2} L ${ix1} ${iy1} A ${ir} ${ir} 0 ${large} 0 ${ix2} ${iy2} Z`;
          svg.appendChild(el("path", { d, fill: color }));
          start = end;
        }
      }
    }

    // Center label
    const t1 = el("text", {
      x: cx, y: cy - 4, "text-anchor": "middle",
      "font-size": "26", "font-weight": "600", fill: "currentColor",
    });
    t1.textContent = total;
    svg.appendChild(t1);
    const t2 = el("text", {
      x: cx, y: cy + 16, "text-anchor": "middle",
      "font-size": "11", fill: "currentColor", opacity: "0.6",
    });
    t2.textContent = "账号";
    svg.appendChild(t2);

    // Legend
    const legend = document.createElement("div");
    legend.className = "donut-legend";
    const labels = {
      healthy: "健康的",
      low_quota: "额度低",
      no_quota: "没有额度",
      banned: "账号被封禁的",
      abnormal: "账号异常的",
    };
    for (const b of buckets) {
      const key = b.status_category || b.confidence;
      const row = document.createElement("div");
      row.className = "legend-row";
      row.innerHTML = `<span class="dot" style="background:${colors[key] || '#999'}"></span><span>${b.status_label || labels[key] || key}</span><b>${b.count}</b>`;
      legend.appendChild(row);
    }
    host.appendChild(legend);
  };

  window.renderSparkline = function (host, samples, opts) {
    if (!samples || !samples.length) {
      host.innerHTML = '<div class="muted small">暂无数据</div>';
      return;
    }
    host.innerHTML = "";
    const W = host.clientWidth || host.offsetWidth || 240;
    const H = opts?.h || 60;
    const key = opts?.key || "EWMAMs";
    const vals = samples.map(s => s[key] || 0);
    const max = Math.max(1, ...vals);
    const stepX = W / Math.max(1, vals.length - 1);

    const svg = makeSvg(W, H, null, {stretch: true});
    host.appendChild(svg);

    if (!vals.some(v => v > 0)) {
      svg.appendChild(el("line", {
        x1: "0", x2: String(W), y1: String(H / 2), y2: String(H / 2),
        stroke: "var(--viz-track,var(--sep))", "stroke-width": "1",
        "stroke-linecap": "round", opacity: "0.72",
      }));
      return;
    }

    let pathD = "";
    for (let i = 0; i < vals.length; i++) {
      const x = i * stepX;
      const y = H - (vals[i] / max) * (H - 4) - 2;
      pathD += (i === 0 ? "M" : "L") + x + "," + y + " ";
    }
    svg.appendChild(el("path", {
      d: pathD, fill: "none", stroke: opts?.color || "var(--accent)",
      "stroke-width": "1.5", "stroke-linejoin": "round", "stroke-linecap": "round",
      opacity: "0.72",
    }));
  };

  window.renderGauge = function (host, value, max, opts) {
    host.innerHTML = "";
    const W = host.clientWidth || host.offsetWidth || 200;
    const H = 120;
    const cx = W / 2, cy = H - 8, r = Math.min(W / 2 - 8, 60);
    const svg = makeSvg(W, H, null, {pxSize: true});
    host.appendChild(svg);

    const start = -Math.PI, end = 0;
    const arcPath = (s, e) => {
      const x1 = cx + Math.cos(s) * r, y1 = cy + Math.sin(s) * r;
      const x2 = cx + Math.cos(e) * r, y2 = cy + Math.sin(e) * r;
      return `M ${x1} ${y1} A ${r} ${r} 0 0 1 ${x2} ${y2}`;
    };
    svg.appendChild(el("path", {
      d: arcPath(start, end), fill: "none",
      stroke: "var(--hairline)", "stroke-width": "12", "stroke-linecap": "round",
    }));
    const frac = Math.max(0, Math.min(1, max ? value / max : 0));
    const filledEnd = start + frac * Math.PI;
    svg.appendChild(el("path", {
      d: arcPath(start, filledEnd), fill: "none",
      stroke: opts?.color || "var(--accent)", "stroke-width": "12", "stroke-linecap": "round",
      opacity: "0.76",
    }));
    const t = el("text", {
      x: cx, y: cy - 8, "text-anchor": "middle",
      "font-size": "20", "font-weight": "600", fill: "currentColor",
    });
    t.textContent = opts?.label || `${Math.round(frac * 100)}%`;
    svg.appendChild(t);
  };

  // appendAudit: base definition (may be overridden by page-level scripts for filtering)
  window.appendAudit = function (ev, target) {
    const host = target || document.getElementById("audit-log");
    if (!host) return;
    const row = document.createElement("div");
    const lvl = ev.level || "info";
    row.className = "audit-row level-" + lvl;
    row.dataset.level = lvl;
    const ts = new Date(ev.at || Date.now()).toLocaleTimeString("zh-CN");
    row.innerHTML = `<span class="audit-ts">${ts}</span><span class="audit-level">${lvl}</span><span class="audit-cat">${ev.category || ""}</span><span class="audit-msg">${escapeHtml(ev.message || "")}</span>`;
    host.prepend(row);
    while (host.children.length > 200) host.removeChild(host.lastChild);
  };

  window.renderCacheKPI = function (host, stat) {
    if (!host || !stat) return;
    host.innerHTML = "";
    const total = stat.Total || 0;
    const hits = stat.Hits || 0;
    const ratio = total > 0 ? hits / total : 0;
    const pct = (ratio * 100).toFixed(1);
    host.innerHTML = `
      <div class="metric ok">${pct}%</div>
      <div class="label">缓存命中率 · ${hits}/${total}</div>
      <div class="cache-bar"><div class="cache-bar-fill" style="width:${pct}%"></div></div>
    `;
  };

  window.renderCacheKeyTable = function (host, rows) {
    if (!host) return;
    host.innerHTML = "";
    if (!rows || rows.length === 0) {
      host.innerHTML = `<div class="muted small center" style="padding:18px">暂无数据（请发出几次请求生成统计）</div>`;
      return;
    }
    const t = document.createElement("table");
    t.className = "data";
    t.innerHTML = `<thead><tr>
      <th>API Key</th><th>租户</th><th>分组</th><th>标签</th>
      <th>请求数</th><th>命中</th><th>命中率</th>
    </tr></thead><tbody></tbody>`;
    const tb = t.querySelector("tbody");
    for (const r of rows) {
      const tr = document.createElement("tr");
      const ratio = r.hit_ratio || 0;
      const pct = (ratio * 100).toFixed(1);
      const colour = ratio > 0.6 ? "var(--ok)" : ratio > 0.3 ? "var(--warn)" : "var(--err)";
      tr.innerHTML = `
        <td><code class="mono small">${truncate(r.api_key, 24)}</code></td>
        <td>${r.tenant_id||"—"}</td>
        <td>${r.group_id||"—"}</td>
        <td>${r.label||"—"}</td>
        <td>${r.total||0}</td>
        <td>${r.hits||0}</td>
        <td>
          <div class="ratio-bar">
            <div class="ratio-fill" style="width:${pct}%;background:${colour}"></div>
            <span class="ratio-label">${pct}%</span>
          </div>
        </td>
      `;
      tb.appendChild(tr);
    }
    host.appendChild(t);
  };

  window.renderCacheSeries = function (host, points) {
    if (!host || !points || !points.length) return;
    host.innerHTML = "";
    const W = host.clientWidth || host.offsetWidth || 300;
    const H = host.clientHeight || host.offsetHeight || 50;

    // If all data is zero, show placeholder instead of flat line
    const hasData = points.some(p => (p.total || 0) > 0);
    if (!hasData) {
      host.innerHTML = '<div style="display:flex;align-items:center;justify-content:center;height:100%;font-size:10px;color:var(--fg-muted);opacity:0.5">暂无缓存数据</div>';
      return;
    }

    const svg = makeSvg(W, H);
    svg.style.display = 'block';
    host.appendChild(svg);
    const pad = 2;
    const innerH = H - pad * 2;
    const stepX = W / Math.max(1, points.length - 1);
    let pTotal = "", pHits = "";
    const max = Math.max(1, ...points.map(p => p.total));
    for (let i = 0; i < points.length; i++) {
      const x = i * stepX;
      const yT = pad + innerH - (points[i].total / max) * innerH;
      const yH = pad + innerH - ((points[i].hits || 0) / max) * innerH;
      pTotal += (i === 0 ? "M" : "L") + x.toFixed(1) + "," + yT.toFixed(1) + " ";
      pHits  += (i === 0 ? "M" : "L") + x.toFixed(1) + "," + yH.toFixed(1) + " ";
    }
    // Fill area under total line
    const fillD = pTotal + "L" + ((points.length-1)*stepX).toFixed(1) + "," + H + " L0," + H + " Z";
    svg.appendChild(el("path", { d: fillD, fill: "var(--fg-muted)", opacity: "0.06" }));
    svg.appendChild(el("path", { d: pTotal, fill: "none", stroke: "var(--fg-muted)", "stroke-width": "1", opacity: "0.4" }));
    svg.appendChild(el("path", { d: pHits, fill: "none", stroke: "var(--viz-ok,var(--ok))", "stroke-width": "1.5", "stroke-linecap": "round", "stroke-linejoin": "round", opacity: "0.72" }));
  };

  function truncate(s, n) {
    if (!s) return "";
    return s.length > n ? s.slice(0, n) + "…" : s;
  }

  window.updateKPIs = function (info) {
    // Pure no-op placeholder; KPIs are server-rendered. Could update count here.
  };

  function escapeHtml(s) {
    return s.replace(/[&<>"']/g, (c) => ({"&":"&amp;","<":"&lt;",">":"&gt;","\"":"&quot;","'":"&#39;"}[c]));
  }

  // ── Account pool card: quota bars + load heatmap ─────────────────────────

  // renderAccountCard draws a rich mini-card for one account slot.
  // slot = { accountID, provider, state, statusCategory, statusLabel, ewmaLatency, inflight,
  //          quotaShortUsed, quotaShortLimit, quotaShortReset,
  //          quotaLongUsed, quotaLongLimit, quotaLongReset,
  //          discoveredModels: [] }
  window.renderAccountCard = function(host, slot) {
    const statusCategory = slot.statusCategory || 'abnormal';
    const statusLabel = slot.statusLabel || ({
      healthy: '健康的',
      low_quota: '额度低',
      no_quota: '没有额度',
      banned: '账号被封禁的',
      abnormal: '账号异常的',
    }[statusCategory] || '账号异常的');
    const statusColor = ({
      healthy: 'var(--ok-quiet,var(--ok))',
      low_quota: 'var(--warn)',
      no_quota: 'var(--err)',
      banned: 'var(--err)',
      abnormal: 'var(--fg-muted)',
    }[statusCategory] || 'var(--fg-muted)');

    const latMs = Math.round(slot.ewmaLatency || 0);
    const inflight = slot.inflight || 0;

    // Load pressure: 0-1 scale based on inflight+latency (calibrated for pool use)
    const pressure = Math.min(1, (inflight / 10) * 0.5 + (latMs / 3000) * 0.3 + (inflight > 0 ? 0.1 : 0));
    const pressureColor = pressure > 0.8 ? 'var(--err)' : pressure > 0.5 ? 'var(--warn)' : 'var(--ok-quiet,var(--ok))';

    const sUsed = slot.quotaShortUsed || 0, sLim = slot.quotaShortLimit || 0;
    const lUsed = slot.quotaLongUsed  || 0, lLim = slot.quotaLongLimit  || 0;
    const sRemain = sLim > 0 ? Math.max(0, Math.min(100, ((sLim - sUsed) / sLim) * 100)) : 100;
    const lRemain = lLim > 0 ? Math.max(0, Math.min(100, ((lLim - lUsed) / lLim) * 100)) : 100;
    const remainColor = (r) => r < 20 ? 'var(--err)' : r < 50 ? 'var(--warn)' : 'var(--ok-quiet,var(--ok))';

    const provClass = 'provider-tag provider-' + (slot.provider || 'openai');
    const modelList = slot.discoveredModels || [];
    const models = modelList.slice(0, 2).join(', ');
    const modelMore = modelList.length > 2 ? ` +${modelList.length - 2}` : '';
    const sReset = slot.quotaShortReset ? fmtRelTime(slot.quotaShortReset) : '—';
    const lReset = slot.quotaLongReset  ? fmtRelTime(slot.quotaLongReset)  : '—';

    host.innerHTML = `
<div style="display:flex;align-items:center;justify-content:space-between;margin-bottom:8px">
  <a href="/accounts/${slot.accountID}" style="font-family:'SF Mono',Menlo,monospace;font-size:11px;font-weight:600;color:var(--fg);text-decoration:none">${slot.accountID}</a>
  <span class="${provClass}" style="font-size:9px">${slot.provider||''}</span>
</div>
<div style="display:flex;gap:6px;align-items:center;margin-bottom:10px">
  <span style="display:inline-block;width:8px;height:8px;border-radius:50%;background:${statusColor};flex-shrink:0"></span>
  <span style="font-size:11px;color:var(--fg-muted)">${statusLabel}</span>
  <span style="margin-left:auto;font-size:11px;font-weight:600;color:${pressureColor}">${latMs}ms</span>
  <span style="font-size:10px;color:var(--fg-muted)">×${inflight}</span>
</div>

<!-- 5h 剩余 -->
<div style="margin-bottom:6px">
  <div style="display:flex;justify-content:space-between;font-size:10px;color:var(--fg-muted);margin-bottom:2px">
    <span>5h 剩余</span>
    <span style="color:${remainColor(sRemain)};font-weight:600">${sLim ? Math.round(sRemain) : '—'}% <span style="color:var(--fg-muted);font-weight:400">↻${sReset}</span></span>
  </div>
  <div style="height:4px;background:var(--sep);border-radius:2px;overflow:hidden">
    <div style="height:100%;width:${Math.round(sRemain)}%;background:${remainColor(sRemain)};border-radius:2px;transition:width 0.5s"></div>
  </div>
</div>

<!-- 7d 剩余 -->
<div style="margin-bottom:8px">
  <div style="display:flex;justify-content:space-between;font-size:10px;color:var(--fg-muted);margin-bottom:2px">
    <span>7d 剩余</span>
    <span style="color:${remainColor(lRemain)};font-weight:600">${lLim ? Math.round(lRemain) : '—'}% <span style="color:var(--fg-muted);font-weight:400">↻${lReset}</span></span>
  </div>
  <div style="height:4px;background:var(--sep);border-radius:2px;overflow:hidden">
    <div style="height:100%;width:${Math.round(lRemain)}%;background:${remainColor(lRemain)};border-radius:2px;transition:width 0.5s"></div>
  </div>
</div>

<!-- Load pressure mini gauge -->
<div style="display:flex;align-items:center;gap:6px;margin-bottom:6px">
  <span style="font-size:10px;color:var(--fg-muted);white-space:nowrap">负载压力</span>
  <div style="flex:1;height:4px;background:var(--sep);border-radius:2px;overflow:hidden">
    <div style="height:100%;width:${Math.round(pressure*100)}%;background:${pressureColor};border-radius:2px"></div>
  </div>
  <span style="font-size:10px;color:${pressureColor};white-space:nowrap">${Math.round(pressure*100)}%</span>
</div>

${models ? `<div style="font-size:10px;color:var(--fg-muted);margin-top:4px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap">${models}${modelMore}</div>` : ''}
`;
  };

  window.renderAccountRow = function(host, slot) {
    const statusCategory = slot.statusCategory || 'abnormal';
    const statusLabel = slot.statusLabel || ({
      healthy: '健康的',
      low_quota: '额度低',
      no_quota: '没有额度',
      banned: '账号被封禁的',
      abnormal: '账号异常的',
    }[statusCategory] || '账号异常的');
    const statusColor = ({
      healthy: 'var(--ok-quiet,var(--ok))',
      low_quota: 'var(--warn)',
      no_quota: 'var(--err)',
      banned: 'var(--err)',
      abnormal: 'var(--fg-muted)',
    }[statusCategory] || 'var(--fg-muted)');

    const latMs = Math.round(slot.ewmaLatency || 0);
    const inflight = slot.inflight || 0;
    const sUsed = slot.quotaShortUsed || 0, sLim = slot.quotaShortLimit || 0;
    const lUsed = slot.quotaLongUsed || 0, lLim = slot.quotaLongLimit || 0;
    const sRemain = sLim > 0 ? Math.max(0, Math.min(100, ((sLim - sUsed) / sLim) * 100)) : 100;
    const lRemain = lLim > 0 ? Math.max(0, Math.min(100, ((lLim - lUsed) / lLim) * 100)) : 100;
    const remainColor = (r) => r < 20 ? 'var(--err)' : r < 50 ? 'var(--warn)' : 'var(--ok-quiet,var(--ok))';
    const sPct = sLim ? Math.round(sRemain) : 0;
    const lPct = lLim ? Math.round(lRemain) : 0;
    const sColor = sLim ? remainColor(sRemain) : 'var(--hairline-2)';
    const lColor = lLim ? remainColor(lRemain) : 'var(--hairline-2)';
    const id = slot.accountID || '';
    const prov = slot.provider || 'unknown';
    const href = '/accounts/' + encodeURIComponent(id);
    host.innerHTML = `
<div class="pool-row-main">
  <span class="pool-row-dot" style="background:${statusColor}"></span>
  <a class="pool-row-id" href="${href}" title="${escapeHtml(id)}">${escapeHtml(id)}</a>
  <span class="provider-tag provider-${escapeHtml(prov)}" style="font-size:9px">${escapeHtml(prov)}</span>
</div>
<div class="pool-row-meta">
  <span class="pool-row-stat" title="${escapeHtml(statusLabel)}">${latMs}ms · ×${inflight}</span>
  <div class="pool-row-bar">
    <span>5h ${sLim ? Math.round(sRemain) + '%' : '—'}</span>
    <div class="pool-row-track"><i style="width:${sPct}%;background:${sColor}"></i></div>
  </div>
  <div class="pool-row-bar">
    <span>7d ${lLim ? Math.round(lRemain) + '%' : '—'}</span>
    <div class="pool-row-track"><i style="width:${lPct}%;background:${lColor}"></i></div>
  </div>
</div>`;
  };

  // renderPoolGrid renders the full accounts grid with cards.
  window.renderPoolGrid = function(host, slots) {
    if (!host) return;
    host.innerHTML = '';
    if (!slots || !slots.length) {
      host.innerHTML = '<div class="empty" style="grid-column:1/-1;padding:34px 18px"><div class="empty-icon">◯</div><div class="empty-title">暂无账号数据</div><div class="empty-sub">添加账号后这里会展示额度、负载与模型能力。</div></div>';
      return;
    }
    const compact = host.classList.contains('compact-pool');
    for (const slot of slots) {
      if (compact) {
        const row = document.createElement('div');
        row.className = 'pool-runtime-row';
        row.tabIndex = 0;
        const href = '/accounts/' + encodeURIComponent(slot.accountID || '');
        row.addEventListener('click', (ev) => {
          if (ev.target.closest('a')) return;
          location.href = href;
        });
        row.addEventListener('keydown', (ev) => {
          if (ev.key === 'Enter' || ev.key === ' ') {
            ev.preventDefault();
            location.href = href;
          }
        });
        renderAccountRow(row, slot);
        host.appendChild(row);
        continue;
      }
      const card = document.createElement('div');
      card.className = 'card pool-runtime-card';
      card.style.cssText = 'padding:14px;cursor:pointer;transition:box-shadow 0.2s';
      card.addEventListener('mouseenter', () => card.style.boxShadow = 'var(--s2)');
      card.addEventListener('mouseleave', () => card.style.boxShadow = '');
      renderAccountCard(card, slot);
      host.appendChild(card);
    }
  };

  // renderGroupPressure renders a provider-grouped pressure bar chart.
  // groups = [{ groupID, provider, accountCount, avgPressure, totalInflight }]
  window.renderGroupPressure = function(host, groups) {
    if (!host || !groups || !groups.length) {
      host.innerHTML = '<div class="empty-sub muted" style="padding:12px">暂无分组数据</div>';
      return;
    }
    host.innerHTML = '';
    for (const g of groups) {
      const pct = Math.min(100, Math.round((g.avgPressure || 0) * 100));
      const color = pct > 70 ? 'var(--err)' : pct > 40 ? 'var(--warn)' : 'var(--ok-quiet,var(--ok))';
      const row = document.createElement('div');
      row.style.cssText = 'display:flex;align-items:center;gap:10px;margin-bottom:10px';
      row.innerHTML = `
<div style="width:80px;font-size:12px;font-weight:500;overflow:hidden;text-overflow:ellipsis;white-space:nowrap" title="${g.groupID}">${g.groupID}</div>
<span class="provider-tag provider-${g.provider}" style="font-size:9px;flex-shrink:0">${g.provider||''}</span>
<div style="flex:1;height:6px;background:var(--sep);border-radius:3px;overflow:hidden">
  <div style="height:100%;width:${pct}%;background:${color};border-radius:3px;transition:width 0.5s"></div>
</div>
<span style="font-size:11px;font-weight:600;color:${color};width:32px;text-align:right">${pct}%</span>
<span style="font-size:10px;color:var(--fg-muted);width:50px;text-align:right">${g.accountCount} 账号</span>`;
      host.appendChild(row);
    }
  };

  // renderCacheRing draws a ring chart for cache hit ratio.
  window.renderCacheRing = function(host, hitRatio, label) {
    host.innerHTML = '';
    const W = host.clientWidth || host.offsetWidth || 120, H = W;
    const cx = W/2, cy = H/2, r = W/2 - 12, ir = r - 16;
    const pct = Math.max(0, Math.min(1, hitRatio || 0));
    const color = pct > 0.6 ? 'var(--viz-ok,var(--ok))' : pct > 0.3 ? 'var(--warn)' : 'var(--err)';

    const svg = makeSvg(W, H, null, {pxSize: true});
    // Background track
    svg.appendChild(el('circle', {cx, cy, r: (r+ir)/2, fill:'none',
      stroke:'var(--sep)', 'stroke-width': r-ir}));
    // Filled arc
    if (pct > 0) {
      const angle = pct * Math.PI * 2;
      const start = -Math.PI/2;
      const end = start + angle;
      const midR = (r+ir)/2;
      const sw = r - ir;
      if (pct >= 1) {
        svg.appendChild(el('circle', {cx, cy, r: midR, fill:'none',
          stroke: color, 'stroke-width': sw}));
      } else {
        const x1=cx+Math.cos(start)*midR, y1=cy+Math.sin(start)*midR;
        const x2=cx+Math.cos(end)*midR,   y2=cy+Math.sin(end)*midR;
        const large = angle > Math.PI ? 1 : 0;
        svg.appendChild(el('path', {
          d: `M${x1},${y1} A${midR},${midR} 0 ${large} 1 ${x2},${y2}`,
          fill:'none', stroke:color, 'stroke-width':sw, 'stroke-linecap':'round'
        }));
      }
    }
    // Center text
    const t1 = el('text', {x:cx, y:cy-4, 'text-anchor':'middle',
      'font-size':'20', 'font-weight':'700', fill:color});
    t1.textContent = Math.round(pct*100)+'%';
    svg.appendChild(t1);
    if (label) {
      const t2 = el('text', {x:cx, y:cy+14, 'text-anchor':'middle',
        'font-size':'10', fill:'currentColor', opacity:'0.55'});
      t2.textContent = label;
      svg.appendChild(t2);
    }
    host.appendChild(svg);
  };

  // renderCapabilityChips renders discovered model chips in a host element.
  window.renderCapabilityChips = function(host, models) {
    host.innerHTML = '';
    if (!models || !models.length) {
      host.innerHTML = '<span class="muted tiny">暂未发现</span>';
      return;
    }
    for (const m of models) {
      const chip = document.createElement('span');
      chip.className = 'chip';
      chip.style.marginRight = '4px';
      chip.style.marginBottom = '4px';
      chip.textContent = m;
      host.appendChild(chip);
    }
  };

  // ── Helper: relative time formatter ──────────────────────────────────────
  function fmtRelTime(iso) {
    const d = new Date(iso);
    const diff = d - Date.now();
    if (isNaN(diff)) return '—';
    const abs = Math.abs(diff);
    const mins = Math.round(abs/60000);
    const hrs  = Math.round(abs/3600000);
    const days = Math.round(abs/86400000);
    const suffix = diff > 0 ? '后' : '前';
    if (mins < 60) return mins + 'm' + suffix;
    if (hrs  < 24) return hrs  + 'h' + suffix;
    return days + 'd' + suffix;
  }

  // appendAudit is defined once above (line ~266) with target+level support.

  // ── Token trend: dual-line chart (input vs output) ──
  window.renderTokenTrend = function (host, data) {
    if (!host || !data || !data.length) return;
    host.innerHTML = "";

    // Handle both {input_tokens, output_tokens} and {input, output} formats
    const hasData = data.some(d => (d.input_tokens || d.input || 0) > 0 || (d.output_tokens || d.output || 0) > 0);
    if (!hasData) {
      host.innerHTML = '<div style="display:flex;align-items:center;justify-content:center;height:100%;font-size:11px;color:var(--fg-muted);opacity:0.5">暂无 Token 数据</div>';
      return;
    }

    host.style.height = "auto";
    const W = host.clientWidth || host.offsetWidth || 600;
    const configuredH = Number(host.dataset.chartHeight || 0);
    const H = configuredH > 0 ? configuredH : Math.max(140, Math.min(220, (host.clientHeight || host.offsetHeight || 200) - 34));
    const compact = host.classList.contains("compact-chart-host") || configuredH > 0;
    const pad = compact ? { l: 46, r: 8, t: 8, b: 18 } : { l: 58, r: 12, t: 12, b: 24 };
    const iW = W - pad.l - pad.r, iH = H - pad.t - pad.b;
    const svg = makeSvg(W, H, null, {pxSize: true});
    svg.style.maxWidth = "100%";
    svg.style.height = H + "px";

    const inTk = data.map(d => d.input_tokens || d.input || 0);
    const outTk = data.map(d => d.output_tokens || d.output || 0);
    const inMax = Math.max(1, ...inTk);
    const outMax = Math.max(1, ...outTk);
    const N = data.length;
    const stepX = iW / Math.max(1, N - 1);

    // Each line uses its own scale so both are clearly visible
    const drawLine = (vals, vMax, color, fillColor) => {
      let d = "";
      const pts = [];
      for (let i = 0; i < N; i++) {
        const x = pad.l + i * stepX;
        const y = pad.t + iH - (vals[i] / vMax) * iH;
        pts.push({ x, y });
        d += (i === 0 ? "M" : "L") + x + "," + y + " ";
      }
      // Filled area
      if (pts.length > 1) {
        const bottom = pad.t + iH;
        let fd = `M${pts[0].x},${bottom}`;
        for (const p of pts) fd += ` L${p.x},${p.y}`;
        fd += ` L${pts[pts.length - 1].x},${bottom} Z`;
        svg.appendChild(el("path", { d: fd, fill: fillColor, opacity: "0.15" }));
      }
      svg.appendChild(el("path", { d, fill: "none", stroke: color, "stroke-width": "2", "stroke-linejoin": "round" }));
    };

    // Y-axis labels for input (left side, dominant scale)
    for (let i = 0; i <= 3; i++) {
      const y = pad.t + iH * i / 3;
      svg.appendChild(el("line", { x1: pad.l, x2: W - pad.r, y1: y, y2: y, stroke: "rgba(127,127,127,0.12)", "stroke-width": "1" }));
      const label = el("text", { x: pad.l - 6, y: y + 4, "text-anchor": "end", "font-size": compact ? "8" : "9", fill: "currentColor", opacity: "0.4" });
      label.textContent = fmtTokens(Math.round(inMax - inMax * i / 3));
      svg.appendChild(label);
    }

    drawLine(inTk, inMax, "var(--accent)", "var(--accent)");
    drawLine(outTk, outMax, "var(--purple)", "var(--purple)");
    host.appendChild(svg);

    const inTotal = inTk.reduce((a, b) => a + b, 0);
    const outTotal = outTk.reduce((a, b) => a + b, 0);
    const legend = document.createElement("div");
    legend.className = "chart-legend-row";
    legend.innerHTML = `<span class="tiny" style="color:var(--accent)">● 输入 ${fmtTokens(inTotal)}</span><span class="tiny" style="color:var(--purple)">● 输出 ${fmtTokens(outTotal)}</span>`;
    host.appendChild(legend);
  };

  // ── Provider pie chart ──
  // data: array of {name, value} OR object {provider: count}
  window.renderProviderPie = function (host, data) {
    if (!host || !data) return;
    host.innerHTML = "";
    let entries;
    if (Array.isArray(data)) {
      entries = data.filter(d => d && d.value > 0).map(d => [d.name || 'unknown', d.value]);
    } else {
      entries = Object.entries(data);
    }
    entries.sort((a, b) => b[1] - a[1]);
    if (!entries.length) {
      host.innerHTML = '<div style="display:flex;align-items:center;justify-content:center;height:100%;font-size:11px;color:var(--fg-muted);opacity:0.5">暂无 Provider 数据</div>';
      return;
    }
    const total = entries.reduce((s, [, v]) => s + v, 0);
    if (total === 0) return;

    host.style.height = "auto";
    const W = host.clientWidth || host.offsetWidth || 200;
    const configuredSize = Number(host.dataset.pieSize || 0);
    const pieSize = Math.min(W, configuredSize > 0 ? configuredSize : 140);
    const cx = pieSize / 2, cy = pieSize / 2, r = pieSize / 2 - 8, ir = r - 20;
    const svg = makeSvg(pieSize, pieSize);

    const provColors = {
      chatgpt: "var(--provider-chatgpt)", openai: "var(--provider-openai)", claude: "var(--provider-claude)",
      gemini: "var(--provider-gemini)", deepseek: "var(--provider-deepseek)", groq: "var(--provider-groq)",
      mistral: "var(--provider-mistral)", cohere: "var(--provider-cohere)", xai: "#888",
    };

    let start = -Math.PI / 2;
    for (const [prov, count] of entries) {
      const frac = count / total;
      const color = provColors[prov] || "#8e8e93";
      if (frac >= 0.999) {
        const midR = (r + ir) / 2;
        svg.appendChild(el("circle", { cx, cy, r: midR, fill: "none", stroke: color, "stroke-width": r - ir }));
      } else {
        const end = start + frac * Math.PI * 2;
        const large = frac > 0.5 ? 1 : 0;
        const x1 = cx + Math.cos(start) * r, y1 = cy + Math.sin(start) * r;
        const x2 = cx + Math.cos(end) * r, y2 = cy + Math.sin(end) * r;
        const ix1 = cx + Math.cos(end) * ir, iy1 = cy + Math.sin(end) * ir;
        const ix2 = cx + Math.cos(start) * ir, iy2 = cy + Math.sin(start) * ir;
        const d = `M ${x1} ${y1} A ${r} ${r} 0 ${large} 1 ${x2} ${y2} L ${ix1} ${iy1} A ${ir} ${ir} 0 ${large} 0 ${ix2} ${iy2} Z`;
        svg.appendChild(el("path", { d, fill: color }));
        start = end;
      }
    }

    const ct = el("text", { x: cx, y: cy + 4, "text-anchor": "middle", "font-size": "16", "font-weight": "600", fill: "currentColor" });
    ct.textContent = total;
    svg.appendChild(ct);
    svg.style.display = 'block';
    svg.style.margin = '0 auto';
    host.appendChild(svg);

    const legend = document.createElement("div");
    legend.className = "donut-legend";
    legend.style.cssText = "grid-template-columns:repeat(auto-fit,minmax(92px,1fr));margin-top:8px";
    for (const [prov, count] of entries) {
      const row = document.createElement("div");
      row.className = "legend-row";
      row.innerHTML = `<span class="dot" style="background:${provColors[prov] || '#8e8e93'}"></span><span>${prov}</span><b>${count}</b>`;
      legend.appendChild(row);
    }
    host.appendChild(legend);
  };

  // ── Horizontal quota bar ──
  window.renderQuotaBar = function (host, used, limit, label) {
    if (!host) return;
    const remain = limit > 0 ? Math.max(0, Math.min(100, Math.round((limit - used) / limit * 100))) : 100;
    const color = remain < 20 ? "var(--err)" : remain < 50 ? "var(--warn)" : "var(--ok-quiet,var(--ok))";
    host.innerHTML = `
      <div style="display:flex;justify-content:space-between;font-size:10px;color:var(--fg-muted);margin-bottom:2px">
        <span>${label || ""}</span><span style="color:${color};font-weight:600">剩余 ${remain}%</span>
      </div>
      <div class="progress-track"><div class="progress-fill" style="width:${remain}%;background:${color}"></div></div>`;
  };

})();
