/* XIMO 中转站管理控制台 —— 前端逻辑
 *
 * 令牌（X-Admin-Token）只存在本机 localStorage，每次请求放在请求头里；
 * 页面本身不需要鉴权，数据接口 /admin/* 由服务端逐个校验。
 */
"use strict";

const $ = (id) => document.getElementById(id);
const TOKEN_KEY = "ximo.admin.token";
let token = localStorage.getItem(TOKEN_KEY) || "";
let users = [];

/* ── 基础 ────────────────────────────────────────────────────────────── */
function toast(msg) {
  const t = $("toast");
  t.textContent = msg;
  t.hidden = false;
  clearTimeout(t._h);
  t._h = setTimeout(() => (t.hidden = true), 2600);
}
function setMsg(id, text, kind) {
  const el = $(id);
  el.textContent = text || "";
  el.className = "msg" + (kind ? " " + kind : "");
}
const esc = (s) => String(s == null ? "" : s).replace(/[&<>"']/g, (c) =>
  ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));

// pickArray：列表接口的形状在不同版本里可能是 {data:[]} / {items:[]} / 裸数组
const pickArray = (j) => (Array.isArray(j) ? j : (j && (j.data || j.items)) || []);
const pick = (j, ...keys) => {
  for (const k of keys) if (j && j[k] !== undefined && j[k] !== null) return j[k];
  return "";
};
const fmtTime = (ms) => {
  const n = Number(ms);
  if (!n) return "-";
  const d = new Date(n);
  return d.toLocaleString("zh-CN", { hour12: false });
};

async function api(path, { method = "GET", body } = {}) {
  const res = await fetch(path, {
    method,
    headers: { "X-Admin-Token": token, "Content-Type": "application/json" },
    body: body ? JSON.stringify(body) : undefined,
  });
  let j = null;
  try { j = await res.json(); } catch (_) { /* 空体 */ }
  if (!res.ok) {
    const m = (j && j.error && j.error.message) || res.statusText;
    const code = (j && j.error && j.error.code) || res.status;
    const err = new Error(`${code}: ${m}`);
    err.status = res.status;
    throw err;
  }
  return j;
}

async function connect(silent) {
  if (!token) { setConn(false, "未连接"); return false; }
  try {
    await api("/admin/users?limit=1");
    localStorage.setItem(TOKEN_KEY, token);
    setConn(true, "已连接");
    if (!silent) toast("已连接");
    return true;
  } catch (e) {
    setConn(false, e.status === 401 ? "令牌无效" : "连接失败");
    if (!silent) setMsg("devMsg", "连接失败：" + e.message, "err");
    return false;
  }
}
function setConn(ok, text) {
  const c = $("connState");
  c.textContent = text;
  c.className = "chip " + (ok ? "ok" : "fail");
}

/* ── 页面切换 ────────────────────────────────────────────────────────── */
const loaders = {};
function show(page) {
  for (const p of document.querySelectorAll(".page")) p.hidden = p.id !== "page-" + page;
  for (const a of document.querySelectorAll(".toc-link")) a.classList.toggle("active", a.dataset.page === page);
  if (loaders[page]) loaders[page]().catch((e) => console.warn(e));
  location.hash = page;
}

/* ── 概览 ────────────────────────────────────────────────────────────── */
loaders.overview = async () => {
  try {
    const h = await (await fetch("/v1/health")).json();
    $("ovHealth").textContent = h.status === "ok" ? "正常" : String(h.status);
    $("ovVersion").textContent = String(h.version || "-");
  } catch (_) { $("ovHealth").textContent = "不可达"; }
  $("gwHint").textContent = location.origin;
  const eps = [
    "GET  /v1/health · /v1/capabilities · /v1/models",
    "POST /v1/chat/completions （OpenAI 兼容，支持 stream:true）",
    "POST /v1/messages （Anthropic 兼容入站）",
    "POST /v1/auth/device · /v1/auth/token · /v1/auth/refresh · /v1/auth/login",
    "GET  /v1/usage",
  ];
  $("ovEndpoints").innerHTML = eps.map((e) => `<li>${esc(e)}</li>`).join("");
  try {
    const [u, m, p] = await Promise.all([
      api("/admin/users?limit=200"), api("/admin/models"), api("/admin/providers"),
    ]);
    $("ovUsers").textContent = pickArray(u).length;
    $("ovModels").textContent = pickArray(m).length;
    const provs = pickArray(p);
    $("ovProviders").textContent = provs.length;
    $("ovUnhealthy").textContent = provs.filter((x) => String(pick(x, "status")) !== "enabled").length;
  } catch (e) {
    $("ovUsers").textContent = e.status === 401 ? "需令牌" : "-";
    $("ovModels").textContent = "需令牌";
    $("ovProviders").textContent = "需令牌";
    $("ovUnhealthy").textContent = "-";
  }
};

/* ── 设备码批准 ──────────────────────────────────────────────────────── */
async function loadUsers() {
  const j = await api("/admin/users?limit=200");
  users = pickArray(j);
  const opts = users.map((u) => {
    const id = pick(u, "id", "user_id");
    return `<option value="${esc(id)}">${esc(pick(u, "username", "id"))} （${esc(id)}）</option>`;
  }).join("");
  $("devUser").innerHTML = opts || '<option value="">（还没有用户，请先到"用户与额度"建一个）</option>';
  $("qUser").innerHTML = opts;
  $("kUser").innerHTML = opts;
  $("aUser").innerHTML = opts;
}
loaders.device = async () => { await loadUsers(); };
$("devApprove").onclick = async () => {
  const code = $("devCode").value.trim();
  const uid = $("devUser").value;
  if (!code) return setMsg("devMsg", "请填写用户码", "err");
  if (!uid) return setMsg("devMsg", "请先建一个用户并选中", "err");
  try {
    const j = await api("/admin/device/approve", { method: "POST", body: { user_code: code, user_id: uid } });
    setMsg("devMsg", `已批准：${pick(j, "status")} · user_code=${pick(j, "user_code")} → ${pick(j, "user_id")}`, "ok");
    toast("已批准，插件将在几秒内完成登录");
  } catch (e) { setMsg("devMsg", "批准失败：" + e.message, "err"); }
};
$("devReload").onclick = () => loadUsers().then(() => setMsg("devMsg", "用户列表已刷新", "ok")).catch((e) => setMsg("devMsg", e.message, "err"));

/* ── 用户与额度 ──────────────────────────────────────────────────────── */
loaders.users = async () => {
  const j = await api("/admin/users?limit=200");
  users = pickArray(j);
  const rows = await Promise.all(users.map(async (u) => {
    const id = pick(u, "id", "user_id");
    let q = null;
    try { q = await api("/admin/quota/accounts/" + encodeURIComponent(id)); } catch (_) { /* 忽略 */ }
    const avail = q ? pick(q, "available") : "-";
    const used = q ? pick(q, "used_amount") : "-";
    const st = String(pick(u, "status"));
    return `<tr>
      <td class="mono">${esc(pick(u, "username", id))}<br><span class="tiny">${esc(id)}</span></td>
      <td>${st === "active" ? '<span class="chip ok">active</span>' : `<span class="chip fail">${esc(st)}</span>`}</td>
      <td class="mono">可用 ${esc(avail)}<br><span class="tiny">已用 ${esc(used)}</span></td>
      <td><button class="btn ghost small" data-toggle="${esc(id)}" data-next="${st === "active" ? "disabled" : "active"}">
        ${st === "active" ? "禁用" : "启用"}</button></td>
    </tr>`;
  }));
  $("uTable").querySelector("tbody").innerHTML = rows.join("") || '<tr><td colspan="4">还没有用户</td></tr>';
  $("uTable").querySelectorAll("tbody button[data-toggle]").forEach((b) => {
    b.onclick = async () => {
      try {
        await api(`/admin/users/${encodeURIComponent(b.dataset.toggle)}/status`, { method: "POST", body: { status: b.dataset.next } });
        await loaders.users(); await loadUsers();
        setMsg("uMsg", "状态已更新", "ok");
      } catch (e) { setMsg("uMsg", e.message, "err"); }
    };
  });
  await loadUsers();
};
$("uReload").onclick = () => loaders.users().then(() => setMsg("uMsg", "已刷新", "ok")).catch((e) => setMsg("uMsg", e.message, "err"));
$("uCreate").onclick = async () => {
  const username = $("uName").value.trim(), password = $("uPass").value;
  if (!username || !password) return setMsg("uMsg", "用户名与密码都要填", "err");
  try {
    const j = await api("/admin/users", { method: "POST", body: { username, password, group_id: "default" } });
    setMsg("uMsg", `已建用户 ${pick(j, "username")} （${pick(j, "id")}）`, "ok");
    $("uName").value = ""; $("uPass").value = "";
    await loaders.users();
  } catch (e) { setMsg("uMsg", e.message, "err"); }
};
$("qDo").onclick = async () => {
  const user_id = $("qUser").value, kind = $("qKind").value;
  const amount = Number($("qAmount").value || 0);
  const reason = $("qReason").value || "console";
  if (!user_id) return setMsg("qMsg", "请选择用户", "err");
  if (!amount) return setMsg("qMsg", "数量要填（微单位）", "err");
  // 幂等键：同一秒内重复点击返回同一笔账本，不重复入账
  const idempotency_key = `console-${kind}-${user_id}-${Date.now()}`;
  try {
    const j = await api("/admin/quota/adjust", { method: "POST", body: { user_id, kind, amount, reason, idempotency_key } });
    const led = j && j.ledger ? j.ledger : j;
    setMsg("qMsg", `已执行：余额后 ${pick(led, "balance_after")}（账本 ${pick(led, "id")}）`, "ok");
    await loaders.users();
  } catch (e) { setMsg("qMsg", e.message, "err"); }
};
$("qLedger").onclick = async () => {
  const uid = $("qUser").value;
  if (!uid) return setMsg("qMsg", "请选择用户", "err");
  try {
    const j = await api(`/admin/quota/ledger?user_id=${encodeURIComponent(uid)}&limit=50`);
    const rows = pickArray(j).map((r) => `<tr>
      <td class="mono">${esc(fmtTime(pick(r, "created_at")))}</td>
      <td>${esc(pick(r, "type"))}</td>
      <td class="mono">${esc(pick(r, "amount"))}</td>
      <td class="mono">${esc(pick(r, "balance_after"))}</td>
      <td>${esc(pick(r, "reason"))}</td></tr>`);
    $("lTable").querySelector("tbody").innerHTML = rows.join("") || '<tr><td colspan="5">没有记录</td></tr>';
  } catch (e) { setMsg("qMsg", e.message, "err"); }
};

/* ── API 密钥 ────────────────────────────────────────────────────────── */
$("kCreate").onclick = async () => {
  const user_id = $("kUser").value;
  if (!user_id) return setMsg("kMsg", "请选择用户", "err");
  const body = { user_id };
  const ttl = Number($("kTtl").value || 0);
  if (ttl > 0) body.ttl_seconds = Math.round(ttl * 86400);
  try {
    const j = await api("/admin/keys", { method: "POST", body });
    const val = pick(j, "api_key", "key");
    $("kOnce").hidden = false;
    $("kOnceVal").textContent = val;
    setMsg("kMsg", "密钥已签发（只显示这一次）", "ok");
    await keyList();
  } catch (e) { setMsg("kMsg", e.message, "err"); }
};
$("kCopy").onclick = async () => {
  try { await navigator.clipboard.writeText($("kOnceVal").textContent); toast("已复制"); }
  catch (_) { toast("复制失败，请手动选择"); }
};
async function keyList() {
  const uid = $("kUser").value;
  if (!uid) {
    $("kTable").querySelector("tbody").innerHTML = '<tr><td colspan="5">请先在上面选择用户（该接口按用户查询）</td></tr>';
    return;
  }
  const j = await api("/admin/keys?limit=200&user_id=" + encodeURIComponent(uid));
  const rows = pickArray(j).map((k) => {
    const id = pick(k, "id");
    const st = String(pick(k, "status"));
    const revoked = st !== "active";
    return `<tr>
      <td class="mono">${esc(pick(k, "key_prefix", "prefix"))}…</td>
      <td class="mono">${esc(pick(k, "user_id"))}</td>
      <td>${revoked ? `<span class="chip fail">${esc(st)}</span>` : '<span class="chip ok">active</span>'}</td>
      <td class="mono">${esc(fmtTime(pick(k, "last_used_at")))}</td>
      <td>${revoked ? "" : `<button class="btn ghost small" data-revoke="${esc(id)}">吊销</button>`}</td></tr>`;
  });
  $("kTable").querySelector("tbody").innerHTML = rows.join("") || '<tr><td colspan="5">没有密钥</td></tr>';
  $("kTable").querySelectorAll("button[data-revoke]").forEach((b) => {
    b.onclick = async () => {
      try {
        await api("/admin/keys/" + encodeURIComponent(b.dataset.revoke), { method: "DELETE" });
        setMsg("kMsg", "已吊销", "ok");
        await keyList();
      } catch (e) { setMsg("kMsg", e.message, "err"); }
    };
  });
}
$("kReload").onclick = () => keyList().then(() => setMsg("kMsg", "已刷新", "ok")).catch((e) => setMsg("kMsg", e.message, "err"));

/* ── 模型与 Provider ─────────────────────────────────────────────────── */
async function renderProviders() {
  const j = await api("/admin/providers");
  const provs = pickArray(j);
  $("pTable").querySelector("tbody").innerHTML = provs.map((p) => `<tr>
    <td class="mono">${esc(pick(p, "id"))}</td>
    <td>${esc(pick(p, "name"))}</td>
    <td class="mono">${esc(pick(p, "endpoint"))}</td>
    <td>${String(pick(p, "status")) === "enabled" ? '<span class="chip ok">enabled</span>' : `<span class="chip fail">${esc(pick(p, "status"))}</span>`}</td>
    <td class="mono tiny">${esc(pick(p, "api_key_ref")) || "（未设）"}</td></tr>`).join("") || '<tr><td colspan="5">还没有 Provider</td></tr>';
  $("mProv").innerHTML = provs.map((p) => `<option value="${esc(pick(p, "id"))}">${esc(pick(p, "id"))}</option>`).join("");
  return provs;
}
async function renderModels() {
  const j = await api("/admin/models");
  $("mTable").querySelector("tbody").innerHTML = pickArray(j).map((m) => {
    const id = pick(m, "id", "model_id");
    return `<tr>
      <td class="mono">${esc(id)}</td>
      <td>${esc(pick(m, "display_name"))}</td>
      <td>${pick(m, "enabled") ? '<span class="chip ok">是</span>' : '<span class="chip">否</span>'}</td>
      <td><button class="btn ghost small" data-map="${esc(id)}">加映射</button></td></tr>`;
  }).join("") || '<tr><td colspan="4">还没有模型</td></tr>';
  $("mTable").querySelectorAll("button[data-map]").forEach((b) => {
    b.onclick = () => {
      $("mId").value = b.dataset.map;
      $("mUpstream").focus();
      setMsg("mMsg", "填上游模型名后点保存，即可把该模型映射到所选 Provider", "warn");
    };
  });
}
loaders.catalog = async () => { await renderProviders(); await renderModels(); };
$("pReload").onclick = () => loaders.catalog().then(() => setMsg("pMsg", "已刷新", "ok")).catch((e) => setMsg("pMsg", e.message, "err"));
$("pCreate").onclick = async () => {
  const id = $("pId").value.trim(), name = $("pName").value.trim(), endpoint = $("pEp").value.trim(), api_key = $("pKey").value.trim();
  if (!id || !endpoint) return setMsg("pMsg", "id 与 endpoint 必填", "err");
  const body = { id, name: name || id, endpoint, protocol: "openai-chat", status: "enabled", weight: 10 };
  if (api_key) body.api_key = api_key;   // 明文只在这一次请求里发，服务端存成 secretref
  try {
    const j = await api("/admin/providers", { method: "POST", body });
    setMsg("pMsg", `已保存 Provider ${pick(j, "id")} · 密钥引用 ${pick(j, "api_key_ref") || "（未设）"}`, "ok");
    $("pKey").value = "";
    await renderProviders();
  } catch (e) { setMsg("pMsg", e.message, "err"); }
};
$("mCreate").onclick = async () => {
  const id = $("mId").value.trim(), upstream = $("mUpstream").value.trim(), prov = $("mProv").value;
  if (!id) return setMsg("mMsg", "模型 id 必填", "err");
  try {
    await api("/admin/models", { method: "POST", body: {
      id, display_name: $("mName").value.trim() || id, capabilities_json: $("mCaps").value.trim() || "{}", enabled: true } });
    if (upstream && prov) {
      await api(`/admin/providers/${encodeURIComponent(prov)}/models`, { method: "POST", body: {
        model_id: id, upstream_model_id: upstream, priority: 1, enabled: true } });
    }
    setMsg("mMsg", upstream && prov ? "模型与映射都已保存" : "模型已保存（未加映射）", "ok");
    await renderModels();
  } catch (e) { setMsg("mMsg", e.message, "err"); }
};

/* ── 用量与审计 ──────────────────────────────────────────────────────── */
$("aUsage").onclick = async () => {
  try {
    const uid = $("aUser").value;
    if (!uid) { setMsg("aMsg", "请先选择用户（用量接口按用户查询）", "err"); return; }
    const j = await api("/admin/usage?limit=100&user_id=" + encodeURIComponent(uid));
    $("usTable").querySelector("tbody").innerHTML = pickArray(j).map((r) => `<tr>
      <td class="mono">${esc(fmtTime(pick(r, "created_at")))}</td>
      <td class="mono tiny">${esc(pick(r, "user_id"))}</td>
      <td class="mono">${esc(pick(r, "model_id"))}</td>
      <td class="mono">${esc(pick(r, "input_tokens"))}/${esc(pick(r, "output_tokens"))}</td>
      <td class="mono">${esc(pick(r, "latency_ms"))}ms</td>
      <td class="mono">${esc(pick(r, "cost_micro"))}</td>
      <td>${esc(pick(r, "status"))}</td></tr>`).join("") || '<tr><td colspan="7">没有记录</td></tr>';
    setMsg("aMsg", "用量已刷新", "ok");
  } catch (e) { setMsg("aMsg", e.message, "err"); }
};
$("aAudit").onclick = async () => {
  try {
    const j = await api("/admin/audit?limit=100");
    $("auTable").querySelector("tbody").innerHTML = pickArray(j).map((r) => `<tr>
      <td class="mono">${esc(fmtTime(pick(r, "created_at")))}</td>
      <td>${esc(pick(r, "actor"))}</td>
      <td class="mono">${esc(pick(r, "action"))}</td>
      <td class="mono tiny">${esc(pick(r, "target"))}</td>
      <td>${String(pick(r, "result")) === "ok" ? '<span class="chip ok">ok</span>' : `<span class="chip fail">${esc(pick(r, "result"))}</span>`}</td>
      <td class="mono tiny">${esc(pick(r, "ip"))}</td></tr>`).join("") || '<tr><td colspan="6">没有记录</td></tr>';
    setMsg("aMsg", "审计已刷新", "ok");
  } catch (e) { setMsg("aMsg", e.message, "err"); }
};
loaders.audit = async () => { await $("aUsage").onclick(); await $("aAudit").onclick(); };

/* ── 启动 ────────────────────────────────────────────────────────────── */
$("connect").onclick = async () => {
  token = $("token").value.trim();
  if (await connect()) await (loaders[location.hash.replace("#", "")] || loaders.overview)();
};
$("token").addEventListener("keydown", (e) => { if (e.key === "Enter") $("connect").onclick(); });
$("forget").onclick = () => {
  localStorage.removeItem(TOKEN_KEY);
  token = ""; $("token").value = "";
  setConn(false, "未连接"); toast("已清除本机保存的令牌");
};
document.querySelectorAll(".toc-link").forEach((a) => {
  a.onclick = (e) => { e.preventDefault(); show(a.dataset.page); };
});

(async function boot() {
  $("token").value = token;
  const page = (location.hash || "#overview").replace("#", "");
  if (await connect(true)) { show(page); } else { show("overview"); }
})();
