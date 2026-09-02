// app.js — swingbot panel'in tek sayfalık istemci mantığı.
//
// SPEC.md Bölüm 7.1/7.2: panel salt-okunuru bir ölçüm cihazıdır. Bu dosya
// hiçbir yazma (POST/PUT/DELETE) isteği yapmaz — sunucudaki her uç nokta
// zaten GET'e kilitlidir (server.go), ama istemci tarafında da hiçbir form
// veya yazma çağrısı yok: onay yalnızca Telegram üzerinden yapılır.
"use strict";

// SPEC.md Bölüm 7.2'nin renk tokenları — app.css ile birebir aynı
// değerler. Sunucu-taraflı SVG (report.go) ve istemci-taraflı chart
// (burada) aynı paletten okusun diye burada da sabitlenir; CSS custom
// property'ler <canvas> tabanlı lightweight-charts'a doğrudan
// aktarılamadığı için bu kopya kaçınılmaz.
const COLORS = {
  ground: "#E6E9E4",
  ink: "#16191C",
  hairline: "#C2C7C1",
  ghost: "#98A199",
  gain: "#1F6F4A",
  loss: "#A32E22",
  pending: "#C9782F",
};

const REDUCE_MOTION = window.matchMedia("(prefers-reduced-motion: reduce)").matches;

// --- fetch / format helpers -------------------------------------------

async function fetchJSON(url) {
  const res = await fetch(url, { headers: { Accept: "application/json" } });
  if (!res.ok) {
    let msg = res.statusText;
    try {
      const body = await res.json();
      if (body && body.error) msg = body.error;
    } catch (_) {
      /* body was not JSON; keep statusText */
    }
    throw new Error(`${res.status} ${msg}`);
  }
  return res.json();
}

function esc(s) {
  return String(s ?? "").replace(/[&<>"']/g, (c) => ({
    "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;",
  }[c]));
}

function fmtNum(n, decimals = 2) {
  if (n === null || n === undefined || Number.isNaN(n)) return "—";
  return Number(n).toLocaleString("en-US", {
    minimumFractionDigits: decimals,
    maximumFractionDigits: decimals,
  });
}

function fmtPct(n, decimals = 2) {
  if (n === null || n === undefined || Number.isNaN(n)) return "—";
  return `${fmtNum(n, decimals)}%`;
}

function fmtQty(q) {
  if (q === null || q === undefined || q === "") return "—";
  return q;
}

function fmtDate(iso) {
  if (!iso) return "—";
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "—";
  return d.toISOString().slice(0, 10);
}

function fmtDateTime(iso) {
  if (!iso) return "—";
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "—";
  return d.toISOString().slice(0, 16).replace("T", " ") + " UTC";
}

function pnlClass(n) {
  if (n === null || n === undefined || Number.isNaN(n)) return "";
  return n > 0 ? "gain" : n < 0 ? "loss" : "";
}

function statusBadgeClass(status) {
  switch (status) {
    case "PENDING":
      return "pending";
    case "APPROVED":
    case "FILLED":
      return "gain";
    case "REJECTED":
    case "FAILED":
    case "EXPIRED":
      return "loss";
    default:
      return "ghost";
  }
}

const app = document.getElementById("app");

// --- router --------------------------------------------------------------

const routes = [
  { pattern: /^\/$/, render: renderOverview },
  { pattern: /^\/positions$/, render: renderPositions },
  { pattern: /^\/proposals$/, render: renderProposals },
  { pattern: /^\/trades$/, render: renderTrades },
  { pattern: /^\/runs$/, render: renderRuns },
  { pattern: /^\/runs\/([^/]+)$/, render: renderRunDetail },
  { pattern: /^\/universe$/, render: renderUniverse },
];

function matchRoute(path) {
  for (const r of routes) {
    const m = path.match(r.pattern);
    if (m) return { render: r.render, params: m.slice(1) };
  }
  return null;
}

function setActiveNav(path) {
  document.querySelectorAll("#nav a").forEach((a) => {
    a.classList.toggle("active", a.getAttribute("data-path") === path);
  });
}

async function renderRoute(path) {
  setActiveNav(path);
  const match = matchRoute(path);
  if (!match) {
    app.innerHTML = `<p class="empty-state">sayfa bulunamadı: ${esc(path)}</p>`;
    return;
  }
  try {
    await match.render(app, ...match.params);
  } catch (err) {
    app.innerHTML = `<p class="empty-state">yüklenemedi: ${esc(err.message || String(err))}</p>`;
  }
}

function navigate(path, push = true) {
  if (push) history.pushState({}, "", path);
  renderRoute(path);
}

document.addEventListener("click", (e) => {
  const a = e.target.closest("a");
  if (!a) return;
  const href = a.getAttribute("href");
  if (!href || !href.startsWith("/") || a.target === "_blank") return;
  e.preventDefault();
  navigate(href);
});

window.addEventListener("popstate", () => renderRoute(location.pathname));

// --- health indicator (polled on every page) ------------------------------

async function refreshHealth() {
  const dot = document.getElementById("health-dot");
  const text = document.getElementById("health-text");
  try {
    const h = await fetchJSON("/api/health");
    dot.classList.toggle("ok", h.dbOk === true && !(h.breaker && h.breaker.open));
    dot.classList.toggle("down", h.dbOk !== true);
    const breakerNote = h.breaker && h.breaker.open ? " · devre kesici AÇIK" : "";
    text.textContent = `${h.mode || "—"}${breakerNote}`;
  } catch (err) {
    dot.classList.remove("ok");
    dot.classList.add("down");
    text.textContent = "erişilemiyor";
  }
}

// --- equity chart (İ3 imza öğesi: strateji + BTC benchmark, hiçbir zaman tek başına) --

function renderEquityChart(container, points) {
  container.innerHTML = "";
  const chartEl = document.createElement("div");
  chartEl.id = "equity-chart";
  container.appendChild(chartEl);

  const legend = document.createElement("div");
  legend.className = "legend";
  legend.innerHTML = `
    <span><span class="swatch" style="background:${COLORS.ink}"></span>strateji</span>
    <span><span class="swatch" style="background:${COLORS.ghost}"></span>BTC al-tut (benchmark)</span>`;
  container.appendChild(legend);

  if (!points || points.length === 0) {
    chartEl.innerHTML = '<p class="empty-state">henüz equity verisi yok</p>';
    return;
  }

  if (!window.LightweightCharts) {
    chartEl.innerHTML = '<p class="empty-state">grafik kütüphanesi yüklenemedi</p>';
    return;
  }

  const chart = window.LightweightCharts.createChart(chartEl, {
    width: chartEl.clientWidth,
    height: 320,
    layout: { background: { color: "transparent" }, textColor: COLORS.ink, fontFamily: "ui-monospace, monospace" },
    grid: {
      vertLines: { color: COLORS.hairline },
      horzLines: { color: COLORS.hairline },
    },
    rightPriceScale: { borderColor: COLORS.hairline },
    timeScale: { borderColor: COLORS.hairline },
    crosshair: { mode: 0 },
    // Hareket eden bir sayı güvenilmez bir sayıdır (SPEC.md Bölüm 7.2):
    // kinetik kaydırma dışındaki tüm animasyonlar kapalı, prefers-reduced-motion'da
    // fare tekerleği/kaydırma da devre dışı bırakılır.
    handleScroll: !REDUCE_MOTION,
    handleScale: !REDUCE_MOTION,
  });

  const toSeriesData = (key) =>
    points
      .filter((p) => p[key] !== null && p[key] !== undefined)
      .map((p) => ({ time: fmtDate(p.ts), value: p[key] }));

  // Benchmark ÖNCE eklenir (arkada durması için): İ3'ün görsel karşılığı
  // — equity eğrisi hiçbir zaman BTC al-tut olmadan çizilmez.
  const benchSeries = chart.addLineSeries({
    color: COLORS.ghost,
    lineWidth: 1,
    priceLineVisible: false,
    lastValueVisible: false,
  });
  benchSeries.setData(toSeriesData("benchBtc"));

  const equitySeries = chart.addLineSeries({
    color: COLORS.ink,
    lineWidth: 2,
    priceLineVisible: false,
  });
  equitySeries.setData(toSeriesData("equity"));

  chart.timeScale().fitContent();

  const resize = () => chart.applyOptions({ width: chartEl.clientWidth });
  window.addEventListener("resize", resize);
}

// --- overview (/) ----------------------------------------------------------

async function renderOverview(root) {
  root.innerHTML = '<p class="empty-state">yükleniyor…</p>';
  const [health, equity, positions, proposals] = await Promise.all([
    fetchJSON("/api/health"),
    fetchJSON("/api/equity"),
    fetchJSON("/api/positions"),
    fetchJSON("/api/proposals?status=PENDING"),
  ]);

  const pts = equity.points || [];
  const last = pts.length ? pts[pts.length - 1] : null;
  const openPositions = positions.positions || [];
  const pending = proposals.proposals || [];
  const breaker = health.breaker || { open: false };

  root.innerHTML = `
    <section class="card grid">
      <div class="metric">
        <span class="label">equity (${esc(equity.mode)})</span>
        <span class="value num">${last ? fmtNum(last.equity, 2) : "—"}</span>
      </div>
      <div class="metric">
        <span class="label">nakit</span>
        <span class="value num">${last ? fmtNum(last.cash, 2) : "—"}</span>
      </div>
      <div class="metric">
        <span class="label">maruziyet</span>
        <span class="value num">${last ? fmtPct(last.exposure * 100) : "—"}</span>
      </div>
      <div class="metric">
        <span class="label">açık pozisyon</span>
        <span class="value num">${openPositions.length}</span>
      </div>
      <div class="metric">
        <span class="label">bekleyen öneri</span>
        <span class="value num ${pending.length ? "pending" : ""}">${pending.length}</span>
      </div>
      <div class="metric">
        <span class="label">devre kesici</span>
        <span class="value num ${breaker.open ? "loss" : "gain"}">${breaker.open ? "AÇIK" : "kapalı"}</span>
      </div>
    </section>

    <section>
      <h2 class="section-title">equity eğrisi — strateji vs. BTC al-tut</h2>
      <div id="equity-chart-host"></div>
    </section>

    <section>
      <h2 class="section-title">açık pozisyonlar</h2>
      ${positionsTable(openPositions)}
    </section>

    <section>
      <h2 class="section-title">bekleyen öneriler</h2>
      ${proposalsTable(pending)}
    </section>
  `;

  renderEquityChart(document.getElementById("equity-chart-host"), pts);
}

// --- positions (/positions) ------------------------------------------------

function positionsTable(positions) {
  if (!positions.length) return '<p class="empty-state">açık pozisyon yok</p>';
  const rows = positions
    .map(
      (p) => `
    <tr>
      <td>${esc(p.symbol)}</td>
      <td>${esc(p.strategy)}</td>
      <td class="num">${fmtQty(p.qty)}</td>
      <td class="num">${fmtNum(p.entryPrice)}</td>
      <td class="num">${p.lastPrice != null ? fmtNum(p.lastPrice) : "—"}</td>
      <td class="num ${pnlClass(p.unrealizedPnl)}">${p.unrealizedPnl != null ? fmtNum(p.unrealizedPnl) : "—"}</td>
      <td class="num ${pnlClass(p.unrealizedPnlPct)}">${p.unrealizedPnlPct != null ? fmtPct(p.unrealizedPnlPct) : "—"}</td>
      <td class="num">${p.stopPrice != null ? fmtNum(p.stopPrice) : "—"}</td>
      <td class="num">${p.stopDistancePct != null ? fmtPct(p.stopDistancePct) : "—"}</td>
      <td class="num">${fmtNum(p.holdDays, 1)}</td>
      <td>${fmtDateTime(p.entryTime)}</td>
    </tr>`
    )
    .join("");
  return `
  <table>
    <thead><tr>
      <th>sembol</th><th>strateji</th><th class="num">miktar</th><th class="num">giriş</th>
      <th class="num">son fiyat</th><th class="num">K/Z</th><th class="num">K/Z %</th>
      <th class="num">stop</th><th class="num">stop mesafesi</th><th class="num">gün</th><th>giriş zamanı</th>
    </tr></thead>
    <tbody>${rows}</tbody>
  </table>`;
}

async function renderPositions(root) {
  root.innerHTML = '<p class="empty-state">yükleniyor…</p>';
  const data = await fetchJSON("/api/positions");
  root.innerHTML = `
    <section>
      <h2 class="section-title">açık pozisyonlar (${esc(data.mode)})</h2>
      ${positionsTable(data.positions || [])}
    </section>`;
}

// --- proposals (/proposals) ------------------------------------------------

function proposalsTable(proposals) {
  if (!proposals.length) return '<p class="empty-state">öneri yok</p>';
  const rows = proposals
    .map(
      (p) => `
    <tr>
      <td>${fmtDateTime(p.createdAt)}</td>
      <td>${esc(p.symbol)}</td>
      <td>${esc(p.side)}</td>
      <td>${esc(p.strategy)}</td>
      <td class="num">${p.score != null ? fmtNum(p.score, 3) : "—"}</td>
      <td class="num">${fmtNum(p.refPrice)}</td>
      <td class="num">${p.stopPrice != null ? fmtNum(p.stopPrice) : "—"}</td>
      <td class="num">${fmtQty(p.qty)}</td>
      <td><span class="badge ${statusBadgeClass(p.status)}">${esc(p.status)}</span></td>
      <td>${esc(p.reason)}</td>
    </tr>`
    )
    .join("");
  return `
  <table>
    <thead><tr>
      <th>oluşturulma</th><th>sembol</th><th>yön</th><th>strateji</th><th class="num">skor</th>
      <th class="num">ref. fiyat</th><th class="num">stop</th><th class="num">miktar</th><th>durum</th><th>gerekçe</th>
    </tr></thead>
    <tbody>${rows}</tbody>
  </table>`;
}

async function renderProposals(root, _params) {
  const params = new URLSearchParams(location.search);
  const status = params.get("status") || "";

  root.innerHTML = `
    <div class="filters">
      <label class="label" for="status-filter">durum</label>
      <select id="status-filter">
        <option value="">tümü</option>
        ${["PENDING", "APPROVED", "REJECTED", "EXPIRED", "SUBMITTED", "FILLED", "FAILED"]
          .map((s) => `<option value="${s}" ${s === status ? "selected" : ""}>${s}</option>`)
          .join("")}
      </select>
    </div>
    <div id="proposals-table"><p class="empty-state">yükleniyor…</p></div>
  `;

  const select = document.getElementById("status-filter");
  select.addEventListener("change", () => {
    const qs = select.value ? `?status=${select.value}` : "";
    history.replaceState({}, "", `/proposals${qs}`);
    loadProposals(select.value);
  });

  async function loadProposals(st) {
    const url = st ? `/api/proposals?status=${encodeURIComponent(st)}` : "/api/proposals";
    const data = await fetchJSON(url);
    document.getElementById("proposals-table").innerHTML = proposalsTable(data.proposals || []);
  }

  await loadProposals(status);
}

// --- trades (/trades) -------------------------------------------------------

function tradesTable(trades) {
  if (!trades.length) return '<p class="empty-state">işlem yok</p>';
  const rows = trades
    .map(
      (t) => `
    <tr>
      <td>${esc(t.symbol)}</td>
      <td>${esc(t.strategy)}</td>
      <td>${fmtDateTime(t.entryTime)}</td>
      <td class="num">${fmtNum(t.entryPrice)}</td>
      <td>${t.exitTime ? fmtDateTime(t.exitTime) : '<span class="badge pending">açık</span>'}</td>
      <td class="num">${t.exitPrice != null ? fmtNum(t.exitPrice) : "—"}</td>
      <td class="num">${fmtQty(t.qty)}</td>
      <td class="num ${pnlClass(t.pnlQuote)}">${t.pnlQuote != null ? fmtNum(t.pnlQuote) : "—"}</td>
      <td class="num ${pnlClass(t.pnlPct)}">${t.pnlPct != null ? fmtPct(t.pnlPct * 100) : "—"}</td>
      <td>${esc(t.exitReason || "—")}</td>
      <td>${esc(t.mode)}</td>
    </tr>`
    )
    .join("");
  return `
  <table>
    <thead><tr>
      <th>sembol</th><th>strateji</th><th>giriş</th><th class="num">giriş fiyatı</th>
      <th>çıkış</th><th class="num">çıkış fiyatı</th><th class="num">miktar</th>
      <th class="num">K/Z</th><th class="num">K/Z %</th><th>gerekçe</th><th>mod</th>
    </tr></thead>
    <tbody>${rows}</tbody>
  </table>`;
}

async function renderTrades(root) {
  const params = new URLSearchParams(location.search);
  const symbol = params.get("symbol") || "";
  const mode = params.get("mode") || "";

  root.innerHTML = `
    <div class="filters">
      <label class="label" for="symbol-filter">sembol</label>
      <input id="symbol-filter" type="text" placeholder="ör. BTC/USDT" value="${esc(symbol)}">
      <label class="label" for="mode-filter">mod</label>
      <select id="mode-filter">
        <option value="">varsayılan</option>
        <option value="backtest" ${mode === "backtest" ? "selected" : ""}>backtest</option>
        <option value="paper" ${mode === "paper" ? "selected" : ""}>paper</option>
        <option value="live" ${mode === "live" ? "selected" : ""}>live</option>
      </select>
    </div>
    <div id="trades-table"><p class="empty-state">yükleniyor…</p></div>
  `;

  const symbolInput = document.getElementById("symbol-filter");
  const modeSelect = document.getElementById("mode-filter");

  async function load() {
    const qs = new URLSearchParams();
    if (symbolInput.value.trim()) qs.set("symbol", symbolInput.value.trim());
    if (modeSelect.value) qs.set("mode", modeSelect.value);
    history.replaceState({}, "", `/trades${qs.toString() ? "?" + qs.toString() : ""}`);
    const data = await fetchJSON(`/api/trades${qs.toString() ? "?" + qs.toString() : ""}`);
    document.getElementById("trades-table").innerHTML = tradesTable(data.trades || []);
  }

  symbolInput.addEventListener("change", load);
  modeSelect.addEventListener("change", load);
  await load();
}

// --- runs (/runs, /runs/{id}) -----------------------------------------------

function metricNumber(metrics, key, decimals = 3) {
  if (!metrics || metrics[key] === undefined || metrics[key] === null) return "—";
  return fmtNum(metrics[key], decimals);
}

async function renderRuns(root) {
  root.innerHTML = '<p class="empty-state">yükleniyor…</p>';
  const data = await fetchJSON("/api/runs");
  const runs = data.runs || [];
  if (!runs.length) {
    root.innerHTML = '<p class="empty-state">henüz koşu yok</p>';
    return;
  }
  const rows = runs
    .map((r) => {
      const m = r.metrics || {};
      return `
    <tr>
      <td><a href="/runs/${encodeURIComponent(r.id)}">${esc(r.id)}</a></td>
      <td>${fmtDateTime(r.createdAt)}</td>
      <td>${esc(r.strategy)}</td>
      <td>${fmtDate(r.startTs)} → ${fmtDate(r.endTs)}</td>
      <td class="num">${metricNumber(m, "TotalReturn")}</td>
      <td class="num">${metricNumber(m, "Sharpe")}</td>
      <td class="num">${metricNumber(m, "MaxDrawdown")}</td>
      <td>${esc((r.gitSha || "—").slice(0, 10))}</td>
    </tr>`;
    })
    .join("");
  root.innerHTML = `
    <table>
      <thead><tr>
        <th>koşu id</th><th>oluşturulma</th><th>strateji</th><th>aralık</th>
        <th class="num">toplam getiri</th><th class="num">sharpe</th><th class="num">max dd</th><th>git sha</th>
      </tr></thead>
      <tbody>${rows}</tbody>
    </table>`;
}

async function renderRunDetail(root, id) {
  root.innerHTML = '<p class="empty-state">yükleniyor…</p>';
  const run = await fetchJSON(`/api/runs/${encodeURIComponent(id)}`);
  const m = run.metrics || {};
  const metricRows = Object.keys(m)
    .filter((k) => k !== "BenchBTC" && k !== "BenchTop10" && typeof m[k] !== "object")
    .map((k) => `<div class="metric"><span class="label">${esc(k)}</span><span class="value num">${fmtNum(m[k], 4)}</span></div>`)
    .join("");

  root.innerHTML = `
    <p><a href="/runs">&larr; koşular</a></p>
    <section class="card">
      <h2 class="section-title">koşu ${esc(run.id)}</h2>
      <p class="num">strateji: ${esc(run.strategy)} · aralık: ${fmtDate(run.startTs)} → ${fmtDate(run.endTs)}${
    run.gitSha ? ` · git: ${esc(run.gitSha)}` : ""
  }</p>
      ${run.reportPath ? `<p>tam rapor: <span class="num">${esc(run.reportPath)}</span></p>` : ""}
    </section>
    <section class="grid">${metricRows || '<p class="empty-state">metrik yok</p>'}</section>
    <section class="card">
      <h2 class="section-title">parametreler</h2>
      <pre class="num">${esc(JSON.stringify(run.params, null, 2))}</pre>
    </section>
    <section class="card">
      <h2 class="section-title">maliyet varsayımları</h2>
      <pre class="num">${esc(JSON.stringify(run.costs, null, 2))}</pre>
    </section>
  `;
}

// --- universe (/universe) ---------------------------------------------------

async function renderUniverse(root) {
  const params = new URLSearchParams(location.search);
  const date = params.get("date") || "";

  root.innerHTML = `
    <div class="filters">
      <label class="label" for="date-filter">tarih</label>
      <input id="date-filter" type="date" value="${esc(date)}">
    </div>
    <div id="universe-body"><p class="empty-state">yükleniyor…</p></div>
  `;

  const dateInput = document.getElementById("date-filter");
  dateInput.addEventListener("change", () => {
    history.replaceState({}, "", `/universe${dateInput.value ? "?date=" + dateInput.value : ""}`);
    load(dateInput.value);
  });

  async function load(d) {
    const url = d ? `/api/universe?date=${encodeURIComponent(d)}` : "/api/universe";
    const data = await fetchJSON(url);
    const included = data.included || [];
    const excluded = data.excluded || [];

    const includedRows = included
      .map((s) => {
        const c = s.components || {};
        return `
      <tr>
        <td class="num">${s.rank}</td>
        <td>${esc(s.symbol)}</td>
        <td class="num">${fmtNum(s.score, 3)}</td>
        <td class="num">${metricNumber(c, "mom_90")}</td>
        <td class="num">${metricNumber(c, "mom_30")}</td>
        <td class="num">${metricNumber(c, "vol_30")}</td>
        <td class="num">${fmtNum(s.medianQuoteVolume30, 0)}</td>
      </tr>`;
      })
      .join("");

    const excludedItems = excluded
      .map((e) => `<dt>${esc(e.symbol)} — ${esc(e.reason)}</dt><dd>${esc(e.detail)}</dd>`)
      .join("");

    document.getElementById("universe-body").innerHTML = `
      <section>
        <h2 class="section-title">evren — ${esc(data.asOf)} (${included.length} sembol)</h2>
        ${
          included.length
            ? `<table>
          <thead><tr>
            <th class="num">sıra</th><th>sembol</th><th class="num">skor</th>
            <th class="num">mom_90</th><th class="num">mom_30</th><th class="num">vol_30</th>
            <th class="num">30g medyan hacim</th>
          </tr></thead>
          <tbody>${includedRows}</tbody>
        </table>`
            : '<p class="empty-state">evrende sembol yok</p>'
        }
      </section>
      <section>
        <h2 class="section-title">elenen semboller (${excluded.length})</h2>
        ${excluded.length ? `<dl class="reason-list">${excludedItems}</dl>` : '<p class="empty-state">eleme yok</p>'}
      </section>
    `;
  }

  await load(date);
}

// --- boot ------------------------------------------------------------------

renderRoute(location.pathname);
refreshHealth();
setInterval(refreshHealth, 15000);
