/* engram-admin SPA */
(() => {
"use strict";

const $ = (s) => document.querySelector(s);
const $$ = (s) => [...document.querySelectorAll(s)];

async function api(path, opts = {}) {
  if (opts.body && typeof opts.body !== "string") {
    opts.body = JSON.stringify(opts.body);
    opts.headers = { "Content-Type": "application/json", ...(opts.headers || {}) };
  }
  const r = await fetch(path, opts);
  if (r.status === 401 && !path.endsWith("/api/login")) {
    showLogin();
    throw new Error("unauthorized");
  }
  const data = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error(data.error || `HTTP ${r.status}`);
  return data;
}

const esc = (s) => String(s ?? "").replace(/[&<>"']/g, (c) =>
  ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));

const fmtTime = (s) => (s || "").replace("T", " ").replace(/\.\d+.*$/, "").replace("Z", "").trim().slice(0, 19);
const fmtDay = (s) => fmtTime(s).slice(0, 10);

/* ─────────── views ─────────── */

const state = { username: "", role: "user", meta: { subtitle: "", mcp_url: "" } };

function showLogin() {
  $("#app-view").classList.add("hidden");
  $("#login-view").classList.remove("hidden");
  setTimeout(() => $("#login-user").focus(), 50);
}

function applyRole() {
  $$("nav a[data-admin]").forEach((a) =>
    a.classList.toggle("hidden", state.role !== "admin"));
}

function showApp() {
  $("#login-view").classList.add("hidden");
  $("#app-view").classList.remove("hidden");
  applyRole();
  route();
}

async function boot() {
  try {
    const meta = await api("/api/meta");
    state.meta = meta;
    $("#login-subtitle").textContent = meta.subtitle;
    $("#join-cmd").textContent =
      `claude mcp add --transport http engram-team ${meta.mcp_url} --header "Authorization: Bearer <token>"`;
  } catch { /* defaults already in markup */ }
  try {
    const me = await api("/api/whoami");
    state.username = me.username; state.role = me.role;
    showApp();
  } catch { showLogin(); }
}

$("#login-btn").onclick = doLogin;
$("#login-pass").addEventListener("keydown", (e) => { if (e.key === "Enter") doLogin(); });
$("#login-user").addEventListener("keydown", (e) => { if (e.key === "Enter") doLogin(); });

async function doLogin() {
  $("#login-err").textContent = "";
  try {
    const d = await api("/api/login", { method: "POST",
      body: { username: $("#login-user").value, password: $("#login-pass").value } });
    state.username = d.username; state.role = d.role;
    $("#login-pass").value = "";
    showApp();
  } catch (e) {
    $("#login-err").textContent = e.message;
  }
}

$("#logout-btn").onclick = async () => { await api("/api/logout", { method: "POST" }); showLogin(); };

/* ─────────── router ─────────── */

const pages = { me: loadMe, dash: loadDash, memory: loadMemory, stats: loadStats, tokens: loadTokens, users: loadUsers };
const adminPages = new Set(["tokens", "users"]);

function route() {
  let p = (location.hash.replace("#/", "") || "me");
  if (!pages[p]) p = "me";
  if (adminPages.has(p) && state.role !== "admin") p = "me";
  $$("nav a").forEach((a) => a.classList.toggle("on", a.dataset.page === p));
  $$(".page").forEach((el) => el.classList.add("hidden"));
  $(`#page-${p}`).classList.remove("hidden");
  pages[p]();
}
window.addEventListener("hashchange", () => { if (!$("#app-view").classList.contains("hidden")) route(); });

/* ─────────── charts ─────────── */

const charts = {};
function chart(id) {
  if (!charts[id]) charts[id] = echarts.init($(id), null, { renderer: "canvas" });
  return charts[id];
}
window.addEventListener("resize", () => Object.values(charts).forEach((c) => c.resize()));

const AX = { axisLine: { lineStyle: { color: "#30363d" } }, axisLabel: { color: "#7d8590" } };
const TIP = { trigger: "axis", backgroundColor: "#1c2128", borderColor: "#30363d", textStyle: { color: "#c9d1d9" } };
const PALETTE = ["#58a6ff", "#3fb950", "#d29922", "#bc8cff", "#39c5cf", "#f778ba", "#f85149", "#7d8590"];

function lineChart(el, days, counts) {
  chart(el).setOption({
    tooltip: TIP, grid: { left: 40, right: 16, top: 20, bottom: 28 },
    xAxis: { type: "category", data: days, ...AX },
    yAxis: { type: "value", minInterval: 1, ...AX, splitLine: { lineStyle: { color: "#21262d" } } },
    series: [{
      type: "line", data: counts, smooth: true, symbol: "none",
      lineStyle: { color: "#58a6ff", width: 2 },
      areaStyle: { color: { type: "linear", x: 0, y: 0, x2: 0, y2: 1,
        colorStops: [{ offset: 0, color: "#1f6feb55" }, { offset: 1, color: "#1f6feb00" }] } },
    }],
  }, true);
}

function pieChart(el, rows) {
  chart(el).setOption({
    tooltip: { ...TIP, trigger: "item" }, color: PALETTE,
    series: [{
      type: "pie", radius: ["42%", "68%"], center: ["50%", "52%"],
      label: { color: "#c9d1d9", fontSize: 12 },
      itemStyle: { borderColor: "#0d1117", borderWidth: 2 },
      data: rows.map((r) => ({ name: r.key, value: r.count })),
    }],
  }, true);
}

function barChart(el, rows) {
  chart(el).setOption({
    tooltip: TIP, grid: { left: 8, right: 30, top: 10, bottom: 6, containLabel: true },
    xAxis: { type: "value", minInterval: 1, ...AX, splitLine: { lineStyle: { color: "#21262d" } } },
    yAxis: { type: "category", data: rows.map((r) => r.key).reverse(), ...AX },
    series: [{
      type: "bar", data: rows.map((r) => r.count).reverse(), barMaxWidth: 22,
      itemStyle: { color: "#1f6feb", borderRadius: [0, 4, 4, 0] },
    }],
  }, true);
}

/* ─────────── page: me (self-service) ─────────── */

const meState = { token: "", revealed: false };

async function loadMe() {
  const d = await api("/api/me");
  meState.token = d.token || "";
  meState.revealed = false;
  renderMeToken(d);
  $("#me-info").innerHTML = `
    <dt>用户名</dt><dd>${esc(d.username)}</dd>
    <dt>角色</dt><dd>${d.role === "admin" ? "管理员" : "普通用户"}</dd>
    <dt>创建时间</dt><dd>${esc(fmtTime(d.created_at))}</dd>
    <dt>最近登录</dt><dd>${d.last_login ? esc(fmtTime(d.last_login)) : "—"}</dd>`;
}

function renderMeToken(d) {
  const has = !!meState.token;
  $("#me-token-row").classList.toggle("hidden", !has);
  $("#me-no-token").classList.toggle("hidden", has);
  $("#me-join-cmd").parentElement && ($("#me-join-cmd").style.display = has ? "" : "none");
  if (!has) return;
  const shown = meState.revealed ? meState.token : "eng_••••••••••••••••";
  $("#me-token").textContent = shown;
  $("#me-token-reveal").textContent = meState.revealed ? "隐藏" : "显示";
  const revoked = d && d.token_revoked;
  $("#me-token-state").innerHTML = revoked
    ? ` <span class="tag off">已吊销</span>` : ` <span class="tag ok">有效</span>`;
  $("#me-join-cmd").textContent =
    `claude mcp add --transport http engram-team ${state.meta.mcp_url} --header "Authorization: Bearer ${meState.token}"`;
}

$("#me-token-reveal").onclick = function () {
  meState.revealed = !meState.revealed;
  const revoked = $("#me-token-state").textContent.includes("吊销");
  renderMeToken(revoked ? { token_revoked: true } : { token_revoked: false });
};
$("#me-token-copy").onclick = function () {
  navigator.clipboard.writeText(meState.token).then(() => (this.textContent = "已复制 ✓"));
};

async function mintOrRegen(confirmMsg) {
  if (confirmMsg && !confirm(confirmMsg)) return;
  try {
    const d = await api("/api/me/token", { method: "POST" });
    meState.token = d.token; meState.revealed = true;
    renderMeToken({ token_revoked: false });
  } catch (e) { alert(e.message); }
}
$("#me-mint").onclick = () => mintOrRegen("");
$("#me-regen").onclick = () =>
  mintOrRegen("重新生成后旧 key 立即失效，你的 agent 要用新命令重新配置。继续？");

$("#pw-change").onclick = async () => {
  const msg = $("#pw-msg");
  msg.textContent = ""; msg.style.color = "";
  try {
    await api("/api/me/password", { method: "POST",
      body: { old_password: $("#pw-old").value, new_password: $("#pw-new").value } });
    msg.textContent = "已修改 ✓"; msg.style.color = "var(--green)";
    $("#pw-old").value = ""; $("#pw-new").value = "";
  } catch (e) { msg.textContent = e.message; msg.style.color = "var(--red)"; }
};

/* ─────────── page: users (admin) ─────────── */

async function loadUsers() {
  const d = await api("/api/users");
  const tb = $("#u-table tbody");
  tb.innerHTML = d.users.length ? d.users.map((u) => `
    <tr>
      <td style="color:var(--bright)">${esc(u.username)}</td>
      <td>${u.role === "admin" ? "管理员" : "普通用户"}</td>
      <td>${u.disabled ? `<span class="tag off">已禁用</span>` : `<span class="tag ok">正常</span>`}</td>
      <td class="muted">${esc(fmtTime(u.created_at))}</td>
      <td class="muted">${u.last_login ? esc(fmtTime(u.last_login)) : "—"}</td>
      <td style="white-space:nowrap">
        ${u.username !== state.username ? `
          ${u.disabled
            ? `<button class="link" data-act="enable" data-n="${esc(u.username)}">启用</button>`
            : `<button class="link" data-act="disable" data-n="${esc(u.username)}">禁用</button>`}
          <button class="link" data-act="reset" data-n="${esc(u.username)}">重置密码</button>
          <button class="link" data-act="del" data-n="${esc(u.username)}" style="color:var(--red)">删除</button>
        ` : `<span class="muted small">当前登录</span>`}
      </td>
    </tr>`).join("")
    : `<tr><td colspan="6" class="muted" style="text-align:center;padding:30px">还没有账号</td></tr>`;
  tb.querySelectorAll("button[data-act]").forEach((b) => (b.onclick = () => userAction(b)));
}

$("#u-create").onclick = async () => {
  const username = $("#u-name").value.trim(), password = $("#u-pass").value;
  if (!username || !password) { alert("用户名和初始密码都要填"); return; }
  try {
    await api("/api/users", { method: "POST", body: { username, password, role: "user" } });
    $("#u-name").value = ""; $("#u-pass").value = "";
    loadUsers();
  } catch (e) { alert(e.message); }
};

async function userAction(b) {
  const name = b.dataset.n, act = b.dataset.act;
  try {
    if (act === "disable" && confirm(`禁用 ${name}？他立即掉线且无法再登录（agent key 不受影响，可另行吊销）。`)) {
      await api(`/api/users/${name}/disable`, { method: "POST" });
    } else if (act === "enable") {
      await api(`/api/users/${name}/enable`, { method: "POST" });
    } else if (act === "reset") {
      const pw = prompt(`给 ${name} 设置新密码（至少 8 位）：`);
      if (pw === null) return;
      await api(`/api/users/${name}/reset-password`, { method: "POST", body: { password: pw } });
      alert("已重置，对方所有会话已强制下线。");
    } else if (act === "del" && confirm(`删除 ${name}？他的账号和 agent key 都会被删除，不可恢复。`)) {
      await api(`/api/users/${name}`, { method: "DELETE" });
    } else { return; }
    loadUsers();
  } catch (e) { alert(e.message); }
}

/* ─────────── page: dash ─────────── */

let tsDays = 30;

async function loadDash() {
  const [o, ts, bd] = await Promise.all([
    api("/api/stats/overview"),
    api(`/api/stats/timeseries?days=${tsDays}`),
    api("/api/stats/breakdown"),
  ]);
  $("#dash-cards").innerHTML = [
    [o.observations, "活跃 memory"],
    [o.sessions, "会话"],
    [o.projects, "项目"],
    [o.prompts, "用户提问"],
    [o.pinned, "置顶"],
    [o.duplicates_sum, "去重拦截"],
    [o.db_size_mb.toFixed(2) + " MB", "库大小"],
    [o.last_write_at ? fmtTime(o.last_write_at).slice(5, 16) : "—", "最近写入"],
  ].map(([v, k]) => `<div class="card"><div class="v">${esc(v)}</div><div class="k">${k}</div></div>`).join("");
  lineChart("#chart-ts", ts.points.map((p) => p.day.slice(5)), ts.points.map((p) => p.count));
  pieChart("#chart-type", bd.by_type);
  barChart("#chart-project", bd.by_project.slice(0, 10));
}

$("#ts-seg").addEventListener("click", (e) => {
  const b = e.target.closest("button"); if (!b) return;
  $$("#ts-seg button").forEach((x) => x.classList.toggle("on", x === b));
  tsDays = +b.dataset.days;
  loadDash();
});

/* ─────────── page: memory ─────────── */

const mem = { page: 1, size: 20, q: "", project: "", type: "" };
const MEM_TYPES = ["", "config", "discovery", "decision", "learning", "bugfix", "pattern", "preference"];

async function loadMemory() {
  const u = new URLSearchParams({ page: mem.page, size: mem.size });
  if (mem.q) u.set("q", mem.q);
  if (mem.project) u.set("project", mem.project);
  if (mem.type) u.set("type", mem.type);
  const d = await api("/api/memories?" + u);

  const projSel = $("#mem-project");
  if (projSel.dataset.filled !== "1") {
    projSel.innerHTML = `<option value="">全部项目</option>` +
      (d.projects || []).map((p) => `<option>${esc(p)}</option>`).join("");
    projSel.dataset.filled = "1";
  }
  const typeSel = $("#mem-type");
  if (typeSel.dataset.filled !== "1") {
    typeSel.innerHTML = MEM_TYPES.map((t) =>
      `<option value="${t}">${t || "全部类型"}</option>`).join("");
    typeSel.dataset.filled = "1";
  }

  const tb = $("#mem-table tbody");
  tb.innerHTML = d.items.length ? d.items.map((m) => `
    <tr data-id="${m.id}">
      <td class="muted">${esc(fmtTime(m.created_at))}</td>
      <td>${esc(m.project)}</td>
      <td><span class="muted">${esc(m.type)}</span></td>
      <td class="t-title">${esc(m.title)}</td>
      <td class="muted">${m.revisions > 1 ? "r" + m.revisions : ""}</td>
      <td>${m.pinned ? "📌" : ""}</td>
    </tr>`).join("")
    : `<tr><td colspan="6" class="muted" style="text-align:center;padding:30px">没有匹配的记录</td></tr>`;
  tb.querySelectorAll("tr[data-id]").forEach((tr) =>
    (tr.onclick = () => openMemory(tr.dataset.id)));

  const pages = Math.max(1, Math.ceil(d.total / mem.size));
  $("#mem-total").textContent = `共 ${d.total} 条`;
  $("#mem-page").textContent = `${mem.page} / ${pages}`;
  $("#mem-prev").disabled = mem.page <= 1;
  $("#mem-next").disabled = mem.page >= pages;
}

$("#mem-search").onclick = () => {
  mem.q = $("#mem-q").value.trim(); mem.project = $("#mem-project").value;
  mem.type = $("#mem-type").value; mem.page = 1; loadMemory();
};
$("#mem-q").addEventListener("keydown", (e) => { if (e.key === "Enter") $("#mem-search").click(); });
$("#mem-prev").onclick = () => { mem.page--; loadMemory(); };
$("#mem-next").onclick = () => { mem.page++; loadMemory(); };

async function openMemory(id) {
  const m = await api("/api/memories/" + id);
  openModal(`
    <h3 style="color:var(--bright);font-size:16px;padding-right:30px">${esc(m.title)}</h3>
    <dl class="kv">
      <dt>项目</dt><dd>${esc(m.project)}</dd>
      <dt>类型 / scope</dt><dd>${esc(m.type)} · ${esc(m.scope)}</dd>
      <dt>topic_key</dt><dd>${m.topic_key ? `<code>${esc(m.topic_key)}</code>` : "—"}</dd>
      <dt>会话</dt><dd><code>${esc(m.session_id)}</code></dd>
      <dt>创建</dt><dd>${esc(fmtTime(m.created_at))}</dd>
      <dt>更新</dt><dd>${esc(fmtTime(m.updated_at))} · 修订 r${m.revisions} · 重复 ×${m.duplicates}</dd>
      <dt>置顶</dt><dd>${m.pinned ? "是" : "否"}</dd>
    </dl>
    <div class="mem-content">${esc(m.content)}</div>`);
}

/* ─────────── page: stats ─────────── */

async function loadStats() {
  const [bd, o, tp] = await Promise.all([
    api("/api/stats/breakdown"), api("/api/stats/overview"), api("/api/stats/topics?limit=20"),
  ]);
  pieChart("#chart-scope", bd.by_scope);
  $("#stat-dup").textContent = o.duplicates_sum;
  $("#topic-table tbody").innerHTML = tp.topics.length ? tp.topics.map((t) => `
    <tr>
      <td><code>${esc(t.topic_key)}</code></td>
      <td class="t-title">${esc(t.title)}</td>
      <td>${esc(t.project)}</td>
      <td>r${t.revisions}</td>
      <td class="muted">×${t.duplicates}</td>
      <td class="muted">${esc(fmtTime(t.updated_at))}</td>
    </tr>`).join("")
    : `<tr><td colspan="6" class="muted" style="text-align:center;padding:30px">还没有带 topic_key 的记录</td></tr>`;
}

/* ─────────── page: tokens ─────────── */

async function loadTokens() {
  const d = await api("/api/tokens");
  const tb = $("#tok-table tbody");
  tb.innerHTML = d.tokens.length ? d.tokens.map((t) => `
    <tr>
      <td style="color:var(--bright)">${esc(t.name)}</td>
      <td class="muted">${esc(t.note || "")}</td>
      <td><code>…${esc(t.suffix)}</code></td>
      <td class="muted">${esc(fmtTime(t.created_at))}</td>
      <td>${t.revoked ? `<span class="tag off">已吊销</span>` : `<span class="tag ok">有效</span>`}</td>
      <td style="white-space:nowrap">
        ${t.revoked
          ? `<button class="link" data-act="unrevoke" data-n="${esc(t.name)}">恢复</button>`
          : `<button class="link" data-act="revoke" data-n="${esc(t.name)}">吊销</button>`}
        <button class="link" data-act="note" data-n="${esc(t.name)}" data-note="${esc(t.note || "")}">备注</button>
        <button class="link" data-act="del" data-n="${esc(t.name)}" style="color:var(--red)">删除</button>
      </td>
    </tr>`).join("")
    : `<tr><td colspan="6" class="muted" style="text-align:center;padding:30px">还没有 token，点上面签发第一个</td></tr>`;
  tb.querySelectorAll("button[data-act]").forEach((b) => (b.onclick = () => tokenAction(b)));
}

$("#tok-create").onclick = async () => {
  const name = $("#tok-name").value.trim();
  if (!name) { $("#tok-name").focus(); return; }
  try {
    const d = await api("/api/tokens", { method: "POST", body: { name, note: $("#tok-note").value.trim() } });
    $("#tok-name").value = ""; $("#tok-note").value = "";
    openModal(`
      <h3 style="color:var(--bright)">已签发：${esc(d.name)}</h3>
      <p class="muted small" style="margin-top:8px">完整 token 只显示这一次，现在复制发给同事。之后只能看到尾号。</p>
      <div class="token-reveal">${esc(d.token)}</div>
      <button onclick="navigator.clipboard.writeText('${esc(d.token)}').then(()=>this.textContent='已复制 ✓')">复制</button>`);
    loadTokens();
  } catch (e) { alert(e.message); }
};

async function tokenAction(b) {
  const name = b.dataset.n, act = b.dataset.act;
  try {
    if (act === "revoke" && confirm(`吊销 ${name} 的 token？对方立即断开，恢复前无法连接。`)) {
      await api(`/api/tokens/${name}/revoke`, { method: "POST" });
    } else if (act === "unrevoke") {
      await api(`/api/tokens/${name}/unrevoke`, { method: "POST" });
    } else if (act === "note") {
      const note = prompt(`修改 ${name} 的备注：`, b.dataset.note || "");
      if (note === null) return;
      await api(`/api/tokens/${name}`, { method: "PATCH", body: { note } });
    } else if (act === "del" && confirm(`彻底删除 ${name} 的 token？不可恢复（可以重新签发）。`)) {
      await api(`/api/tokens/${name}`, { method: "DELETE" });
    } else { return; }
    loadTokens();
  } catch (e) { alert(e.message); }
}

/* ─────────── modal ─────────── */

function openModal(html) {
  $("#modal-body").innerHTML = html;
  $("#modal-mask").classList.remove("hidden");
}
$("#modal-close").onclick = () => $("#modal-mask").classList.add("hidden");
$("#modal-mask").addEventListener("click", (e) => {
  if (e.target === $("#modal-mask")) $("#modal-mask").classList.add("hidden");
});
document.addEventListener("keydown", (e) => {
  if (e.key === "Escape") $("#modal-mask").classList.add("hidden");
});

boot();
})();
