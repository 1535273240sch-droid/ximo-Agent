/* ============================================================================
   ximo-plugin 控制台前端
   契约：recon/插件UI契约-冻结.md §2（令牌）§3（接口）§4（视觉）

   硬性约束：
   - 无框架、无构建步骤、离线；不引 CDN / 字体 / 图片，唯一外部交互是同源 /api/*。
   - 令牌从 URL 的 ?t= 读取，之后每个 /api/* 请求都带 X-UI-Token。
   - 页面上任何位置都不出现未掩码的密钥：所有要显示的字符串都过 maskText()。
   - 所有数据都来自真实接口，本文件内不存放任何演示数据。
   ========================================================================== */
(function () {
  'use strict';

  var TOKEN_STORAGE_KEY = 'ximo.plugin.ui.token';
  var PAGES = ['detect', 'connect', 'models', 'apply', 'doctor', 'adapters'];
  var REQUEST_TIMEOUT_MS = 25000;
  // 设备码轮询节奏：后端会回服务端给的 interval（秒，1..60），没给就用 5s
  // （与 internal/gateway 的 defaultPollInterval 一致）。
  var LOGIN_POLL_FALLBACK_MS = 5000;
  var LOGIN_DEFAULT_BUDGET_S = 300;

  /* ══ 1. 会话令牌 ═══════════════════════════════════════════════════════ */
  // 优先读 URL 的 ?t=；读到就暂存到 sessionStorage，刷新后仍可用。
  // sessionStorage 只在当前标签页内可见，且令牌从不写进 DOM 文本。
  function readToken() {
    var fromUrl = '';
    try {
      var params = new URLSearchParams(window.location.search);
      fromUrl = params.get('t') || '';
    } catch (e) { fromUrl = ''; }
    if (fromUrl) {
      try { window.sessionStorage.setItem(TOKEN_STORAGE_KEY, fromUrl); } catch (e) { /* 隐私模式忽略 */ }
      return fromUrl;
    }
    try { return window.sessionStorage.getItem(TOKEN_STORAGE_KEY) || ''; } catch (e) { return ''; }
  }
  var uiToken = readToken();

  /* ══ 2. 掩码（前端兜底，后端已掩码一次） ═════════════════════════════ */
  // 与后端 internal/gateway.Mask 同形：保留前后各 4 位，中间 ****。
  function maskSecret(s) {
    var v = String(s == null ? '' : s);
    if (!v) { return '(未配置)'; }
    if (v.length <= 8) { return '****'; }
    return v.slice(0, 4) + '****' + v.slice(-4);
  }

  // 已知前缀的密钥形态：直接掩码。
  var SECRET_SHAPES = [
    /ximo_sk_[A-Za-z0-9_-]+/g,
    /gwa_[A-Za-z0-9_-]+/g,
    /sk-[A-Za-z0-9_-]{8,}/g,
    /Bearer\s+[A-Za-z0-9._-]{8,}/g,
    /\bgw_[A-Za-z0-9_-]{12,}/g
  ];
  // 未知形态的长串：≥32 位且同时含字母与数字才掩码，避免把路径/描述切碎。
  var LONG_RUN = /[A-Za-z0-9_-]{32,}/g;

  function maskText(value) {
    var out = String(value == null ? '' : value);
    var i;
    for (i = 0; i < SECRET_SHAPES.length; i++) {
      out = out.replace(SECRET_SHAPES[i], function (m) { return maskSecret(m); });
    }
    return out.replace(LONG_RUN, function (m) {
      if (!/[0-9]/.test(m) || !/[A-Za-z]/.test(m)) { return m; }
      return maskSecret(m);
    });
  }

  /* ══ 3. DOM 小工具 ═══════════════════════════════════════════════════ */
  function $(sel, root) { return (root || document).querySelector(sel); }
  function $$(sel, root) {
    return Array.prototype.slice.call((root || document).querySelectorAll(sel));
  }
  function el(tag, cls, text) {
    var node = document.createElement(tag);
    if (cls) { node.className = cls; }
    if (text !== undefined && text !== null) { node.textContent = maskText(text); }
    return node;
  }
  // 原文写入（仅用于已经过 maskText 的字符串，或本来就不是秘密的用户码等）
  function raw(node, text) { node.textContent = String(text == null ? '' : text); return node; }
  function clear(node) { while (node.firstChild) { node.removeChild(node.firstChild); } return node; }

  function badge(kind, text) {
    var order = { pass: 1, warn: 2, fail: 3, neutral: 4 };
    var cls = order[kind] ? kind : 'neutral';
    return el('span', 'badge badge-' + cls, text);
  }

  /* ══ 4. 错误与接口客户端 ═════════════════════════════════════════════ */
  function UiError(message, code, status) {
    this.name = 'UiError';
    this.message = String(message || '未知错误');
    this.code = code || '';
    this.status = status || 0;
  }
  UiError.prototype = Object.create(Error.prototype);
  UiError.prototype.constructor = UiError;

  function errorText(err) {
    if (!err) { return '未知错误'; }
    if (err.message) { return String(err.message); }
    return String(err);
  }

  function parseEnvelope(res, rawBody) {
    var env = null;
    if (rawBody) {
      try { env = JSON.parse(rawBody); } catch (e) { env = null; }
    }
    var isEnvelope = !!env && typeof env === 'object' && !Array.isArray(env) && typeof env.ok === 'boolean';
    if (!res.ok) {
      var msg;
      if (isEnvelope && env.error && env.error.message) {
        msg = env.error.message;
      } else if (rawBody && !isEnvelope) {
        msg = 'HTTP ' + res.status + '：' + String(rawBody).slice(0, 300);
      } else {
        msg = 'HTTP ' + res.status;
      }
      if (res.status === 401) {
        msg += '（会话令牌无效或已失效：请用 `ximo-plugin ui` 启动时打印的完整地址（带 ?t= 参数）重新打开本页面）';
      }
      throw new UiError(msg, isEnvelope && env.error ? env.error.code : '', res.status);
    }
    if (isEnvelope) {
      if (!env.ok) {
        throw new UiError(
          (env.error && env.error.message) || '接口返回 ok=false，但未给出原因',
          env.error && env.error.code, res.status
        );
      }
      return env.data === undefined ? null : env.data;
    }
    // 后端理论上总带信封；没有信封时按裸 JSON 处理，也不编造内容。
    return env;
  }

  function api(path, options) {
    options = options || {};
    var timeoutMs = options.timeout || REQUEST_TIMEOUT_MS;
    var controller = null;
    var timer = null;
    var init = {
      method: options.method || 'GET',
      headers: { 'Accept': 'application/json', 'X-UI-Token': uiToken },
      cache: 'no-store',
      credentials: 'same-origin'
    };
    if (options.body !== undefined) {
      init.headers['Content-Type'] = 'application/json';
      init.body = JSON.stringify(options.body);
    }
    if (typeof window.AbortController === 'function') {
      controller = new window.AbortController();
      init.signal = controller.signal;
      timer = window.setTimeout(function () { controller.abort(); }, timeoutMs);
    }
    function done() {
      if (timer !== null) { window.clearTimeout(timer); timer = null; }
    }
    return window.fetch(path, init).then(function (res) {
      return res.text().then(function (body) { return [res, body]; });
    }, function (err) {
      done();
      if (err && err.name === 'AbortError') {
        throw new UiError('请求超时（' + timeoutMs + ' ms）：' + path);
      }
      throw new UiError('无法连接本地服务（' + path + '）：' + (err && err.message ? err.message : String(err)));
    }).then(function (pair) {
      done();
      return parseEnvelope(pair[0], pair[1]);
    });
  }

  /* ══ 5. 信封字段容错读取 ═════════════════════════════════════════════ */
  function pickArray(data, keys) {
    if (Array.isArray(data)) { return data; }
    if (data && typeof data === 'object') {
      var i;
      for (i = 0; i < keys.length; i++) {
        if (Array.isArray(data[keys[i]])) { return data[keys[i]]; }
      }
    }
    return [];
  }
  function pickString(data, keys) {
    if (data && typeof data === 'object') {
      var i;
      for (i = 0; i < keys.length; i++) {
        if (typeof data[keys[i]] === 'string') { return data[keys[i]]; }
      }
    }
    return '';
  }

  /* ══ 6. 全局提示区 ═══════════════════════════════════════════════════ */
  function notify(kind, text) {
    var box = $('#global-message');
    if (!box) { return; }
    var level = kind === 'error' ? 'assertive' : 'polite';
    box.setAttribute('aria-live', level);
    box.className = 'message is-' + (kind || 'info');
    raw(box, maskText(text));
    box.hidden = false;
  }
  function clearNotify() {
    var box = $('#global-message');
    if (!box) { return; }
    box.hidden = true;
    raw(box, '');
  }
  function setHint(id, text, isError) {
    var node = document.getElementById(id);
    if (!node) { return; }
    node.className = isError ? 'hint is-error' : 'hint';
    raw(node, maskText(text));
  }

  /* ══ 7. 全局状态 ═════════════════════════════════════════════════════ */
  var state = {
    version: '', home: '', isolatedHome: false, gateway: '',
    loggedIn: false, credMasked: '', adapters: []
  };
  var cache = { models: null, specs: null };
  var loadedPages = {};

  function normalizeAdapter(a) {
    a = a || {};
    return {
      id: pickString(a, ['id', 'spec_id']),
      name: pickString(a, ['name']) || pickString(a, ['id', 'spec_id']),
      detected: !!(a.detected || a.found),
      evidence: Array.isArray(a.evidence) ? a.evidence : []
    };
  }

  function applyStateData(data) {
    if (data && typeof data === 'object') {
      state.version = typeof data.version === 'string' ? data.version : state.version;
      state.home = typeof data.home === 'string' ? data.home : state.home;
      state.isolatedHome = !!data.isolated_home;
      state.gateway = typeof data.gateway === 'string' ? data.gateway : state.gateway;
      state.loggedIn = !!data.logged_in;
      state.credMasked = typeof data.cred_masked === 'string' ? data.cred_masked : '';
      state.adapters = pickArray(data.adapters, ['adapters']).map(normalizeAdapter);
    }
    renderStatusChips();
    // /api/state 也带回适配器列表：应用页可能在 state 到达之前就已经渲染过下拉框，
    // 这里补一次，避免直接打开 #apply 时看到空列表。
    renderApplySpecOptions();
  }

  function chip(label, value, mono) {
    var li = el('li', 'chip');
    li.appendChild(el('span', 'chip-label', label));
    li.appendChild(el('span', mono ? 'chip-value mono' : 'chip-value', value));
    return li;
  }

  function renderStatusChips() {
    var host = clear($('#status-chips'));
    host.appendChild(chip('版本', state.version || '—'));
    host.appendChild(chip('主目录', state.home || '—', true));
    if (state.isolatedHome) { host.appendChild(chip('隔离主目录', '是（--home 生效）')); }
    host.appendChild(chip('网关', state.gateway || '（未配置）', true));
    host.appendChild(chip('登录', state.loggedIn ? '已登录' : '未登录'));
    host.appendChild(chip('凭据', state.credMasked ? state.credMasked : '（未配置）', true));
    var line = $('#env-line');
    if (line) {
      var parts = [];
      if (state.version) { parts.push('v' + state.version); }
      if (state.home) { parts.push('home=' + state.home); }
      raw(line, maskText(parts.join(' · ') || '状态未知'));
    }
  }

  function loadState() {
    return api('/api/state').then(function (data) {
      applyStateData(data);
      renderConnect();
      return data;
    });
  }

  /* ══ 8. 路由 ═════════════════════════════════════════════════════════ */
  function currentPage() {
    var hash = String(window.location.hash || '').replace(/^#/, '');
    return PAGES.indexOf(hash) >= 0 ? hash : 'detect';
  }

  function showPage(name, isFirst) {
    PAGES.forEach(function (p) {
      var section = document.querySelector('[data-page="' + p + '"]');
      if (section) { section.hidden = (p !== name); }
    });
    $$('.toc-link').forEach(function (link) {
      if (link.getAttribute('data-nav') === name) { link.setAttribute('aria-current', 'page'); }
      else { link.removeAttribute('aria-current'); }
    });
    if (!isFirst) {
      // 切换页面后把焦点交给新页面的标题，键盘/读屏用户才知道内容变了。
      var heading = $('#page-' + name + ' h2');
      if (heading) {
        heading.setAttribute('tabindex', '-1');
        heading.focus();
      }
    }
    var autoload = { detect: true, models: true, apply: true, doctor: true, adapters: true };
    if (!loadedPages[name] && autoload[name] && uiToken) {
      loadedPages[name] = true;
      var loader = pageLoaders[name];
      if (loader) {
        // 加载器自己会在页面上渲染错误；这里吞掉 rejection，避免 unhandled rejection。
        try {
          var pending = loader();
          if (pending && typeof pending.catch === 'function') { pending.catch(function () {}); }
        } catch (e) { /* 加载器自身抛错已在页面上体现 */ }
      }
    }
  }

  /* ══ 9. 检测页 ═══════════════════════════════════════════════════════ */
  function kindLabel(kind) {
    switch (kind) {
      case 'binary': return '可执行';
      case 'path': return '路径';
      case 'env': return '环境变量';
      case 'file': return '文件';
      case 'ipc': return 'IPC';
      default: return kind || '其它';
    }
  }

  function evidenceItem(e) {
    e = e || {};
    var li = el('li', e.ok ? 'evidence-item ok' : 'evidence-item no');
    li.appendChild(el('span', 'evidence-kind', kindLabel(e.kind)));
    var body = el('span', 'evidence-body');
    body.appendChild(el('code', 'mono', e.need || ''));
    if (e.resolved) { body.appendChild(el('span', 'evidence-resolved', '→ ' + e.resolved)); }
    if (e.detail) { body.appendChild(el('span', 'evidence-detail', e.detail)); }
    if (!e.need && !e.detail && !e.resolved) { body.appendChild(el('span', 'evidence-detail', '(证据字段为空)')); }
    li.appendChild(body);
    return li;
  }

  function renderDetect(list) {
    var host = clear($('#detect-list'));
    var normalized = (list || []).map(normalizeAdapter);
    if (!normalized.length) {
      host.appendChild(el('div', 'empty', '没有加载到任何适配器规格（内置库为空，或后端返回了空列表）。'));
      return;
    }
    normalized.forEach(function (a) {
      var card = el('article', 'card');
      var head = el('div', 'card-head');
      head.appendChild(el('h3', 'card-title', a.name || '(未命名适配器)'));
      head.appendChild(el('span', 'mono id-tag', a.id));
      head.appendChild(badge(a.detected ? 'pass' : 'warn', a.detected ? '已检测到' : '未检测到'));
      card.appendChild(head);
      if (a.evidence.length) {
        var ul = el('ul', 'evidence');
        a.evidence.forEach(function (e) { ul.appendChild(evidenceItem(e)); });
        card.appendChild(ul);
      } else {
        card.appendChild(el('p', 'hint', '该适配器没有声明任何检测证据。'));
      }
      host.appendChild(card);
    });
  }

  function runDetect() {
    setHint('detect-hint', '正在检测本机…', false);
    return api('/api/detect', { method: 'POST' }).then(function (data) {
      var list = pickArray(data, ['adapters', 'detections', 'items']);
      renderDetect(list);
      state.adapters = list.map(normalizeAdapter);
      var found = state.adapters.filter(function (a) { return a.detected; }).length;
      setHint('detect-hint',
        '共 ' + state.adapters.length + ' 个适配器，其中 ' + found + ' 个在本机检测到。', false);
      renderApplySpecOptions();
    }, function (err) {
      setHint('detect-hint', '检测失败：' + errorText(err), true);
      notify('error', '重新检测失败：' + errorText(err));
    });
  }

  /* ══ 10. 连接网关页 ══════════════════════════════════════════════════ */
  function kvRow(dl, term, value, mono) {
    dl.appendChild(el('dt', null, term));
    var dd = el('dd', mono ? 'mono' : null, value);
    dl.appendChild(dd);
    return dd;
  }

  function renderConnect() {
    var dl = clear($('#connect-kv'));
    if (!dl) { return; }
    kvRow(dl, '网关地址', state.gateway || '（未配置：请用 `ximo-plugin ui --gateway <url>` 启动）', true);
    kvRow(dl, '登录状态', state.loggedIn ? '已登录' : '未登录', false);
    kvRow(dl, '本机凭据', state.credMasked || '（未配置）', true);
    kvRow(dl, '主目录', state.home || '—', true);
    kvRow(dl, '主目录隔离', state.isolatedHome ? '是（--home 生效，不会碰真实用户目录）' : '否（就是当前用户的真实主目录）', false);
  }

  var login = { timer: null, deadline: 0, busy: false, code: '' };

  function setLoginButtons(running) {
    var poll = $('[data-action="login-poll"]');
    var stop = $('[data-action="login-stop"]');
    var start = $('[data-action="login-start"]');
    if (poll) { poll.hidden = !running; }
    if (stop) { stop.hidden = !running; }
    if (start) { start.disabled = running; }
  }

  function stopLogin(reason) {
    if (login.timer !== null) { window.clearInterval(login.timer); login.timer = null; }
    login.busy = false;
    setLoginButtons(false);
    if (reason) { setHint('login-status', reason, false); }
  }

  function startLogin() {
    stopLogin('');
    setHint('login-status', '正在向网关申请设备码…', false);
    var panel = $('#login-panel');
    api('/api/login/start', { method: 'POST' }).then(function (data) {
      var code = pickString(data, ['user_code']);
      var uri = pickString(data, ['verification_uri', 'verification_url']);
      var expiresIn = typeof data.expires_in === 'number' ? data.expires_in : LOGIN_DEFAULT_BUDGET_S;
      var intervalS = typeof data.interval === 'number' && data.interval > 0
        ? Math.min(60, Math.max(1, Math.round(data.interval))) : 0;
      if (!code) {
        stopLogin('');
        notify('error', '后端没有返回 user_code，无法完成设备码登录。');
        return;
      }
      login.code = code;
      panel.hidden = false;
      // 用户码是给用户照着念/输入的展示值，不是密钥，原样显示。
      raw($('#login-code'), code);
      var link = $('#login-verify');
      if (uri) { link.href = uri; link.hidden = false; raw(link, '打开验证地址'); }
      else { link.hidden = true; link.removeAttribute('href'); }
      setLoginButtons(true);
      notify('info', '设备码已就绪：请在验证页面输入用户码，本页会自动轮询。');
      login.deadline = Date.now() + Math.max(30, expiresIn) * 1000;
      login.timer = window.setInterval(pollLogin,
        intervalS > 0 ? intervalS * 1000 : LOGIN_POLL_FALLBACK_MS);
      pollLogin();
    }, function (err) {
      stopLogin('');
      setHint('login-status', '申请设备码失败：' + errorText(err), true);
      notify('error', '申请设备码失败：' + errorText(err));
    });
  }

  function pollLogin() {
    if (login.busy) { return; }
    if (Date.now() > login.deadline) {
      stopLogin('用户码已过期，请重新开始设备码登录。');
      notify('warn', '设备码已过期（超时未确认），请重新点「开始设备码登录」。');
      return;
    }
    login.busy = true;
    api('/api/login/poll', { method: 'POST' }).then(function (data) {
      login.busy = false;
      var status = pickString(data, ['status']);
      var masked = pickString(data, ['cred_masked']);
      var warning = pickString(data, ['warning']);
      if (status === 'ok') {
        stopLogin('');
        if (warning) {
          setHint('login-status', '登录成功（' + (masked || '已掩码') + '），但有提示：' + warning, true);
          notify('warn', '登录成功，但后端报告：' + warning);
        } else {
          setHint('login-status', '登录成功，本机凭据已保存（' + (masked || '已掩码') + '）。', false);
          notify('ok', '登录成功。凭据已写入本机凭据文件（页面只显示掩码）。');
        }
        loadState();
        return;
      }
      var left = Math.max(0, Math.round((login.deadline - Date.now()) / 1000));
      setHint('login-status', '等待确认中（状态：' + (status || '未知') + '，剩余约 ' + left + ' 秒）…', false);
    }, function (err) {
      login.busy = false;
      stopLogin('轮询失败：' + errorText(err));
      notify('error', '轮询登录状态失败：' + errorText(err));
    });
  }

  /* ══ 11. 模型页 ══════════════════════════════════════════════════════ */
  function capabilityTags(caps) {
    var box = el('div', 'tag-row');
    if (!caps || typeof caps !== 'object') {
      box.appendChild(el('span', 'tag tag-off', '未声明'));
      return box;
    }
    var names = [['stream', '流式'], ['vision', '视觉'], ['tools', '工具'], ['reasoning', '推理']];
    var any = false;
    names.forEach(function (n) {
      if (caps[n[0]]) { any = true; box.appendChild(el('span', 'tag', n[1])); }
    });
    if (!any) { box.appendChild(el('span', 'tag tag-off', '未声明')); }
    return box;
  }

  function renderModels(list) {
    var tbody = clear($('#models-table tbody'));
    var emptyBox = $('#models-empty');
    if (!list.length) {
      emptyBox.hidden = false;
      raw(emptyBox, '网关返回了空模型列表：请在网关侧启用模型与上游。');
      return;
    }
    emptyBox.hidden = true;
    list.forEach(function (m) {
      var tr = el('tr');
      tr.appendChild(el('td', 'col-id', m.id));
      tr.appendChild(el('td', null, m.display_name || '—'));
      tr.appendChild(el('td', null, m.provider || '—'));
      var proto = el('td');
      var protoRow = el('div', 'tag-row');
      var protocols = Array.isArray(m.protocols) ? m.protocols : [];
      if (protocols.length) {
        protocols.forEach(function (p) { protoRow.appendChild(el('span', 'tag', p)); });
      } else {
        protoRow.appendChild(el('span', 'tag tag-off', '未声明'));
      }
      proto.appendChild(protoRow);
      tr.appendChild(proto);
      var capTd = el('td');
      capTd.appendChild(capabilityTags(m.capabilities));
      tr.appendChild(capTd);
      tbody.appendChild(tr);
    });
  }

  function loadModels(force) {
    if (cache.models && !force) { renderModels(cache.models); return Promise.resolve(cache.models); }
    setHint('models-hint', '正在读取模型列表…', false);
    return api('/api/models').then(function (data) {
      var list = pickArray(data, ['data', 'models', 'items']);
      cache.models = list;
      renderModels(list);
      setHint('models-hint', '共 ' + list.length + ' 个可用模型。', false);
      renderApplyModelOptions();
      return list;
    }, function (err) {
      cache.models = null;
      setHint('models-hint', '读取失败：' + errorText(err), true);
      renderModels([]);
      raw($('#models-empty'), '读取模型失败：' + maskText(errorText(err)));
      $('#models-empty').hidden = false;
      notify('error', '读取模型列表失败：' + errorText(err));
      throw err;
    });
  }

  /* ══ 12. 应用页 ══════════════════════════════════════════════════════ */
  var applyState = { specID: '', model: '', key: '', planned: false };

  function invalidatePlan() {
    if (!applyState.planned) { return; }
    applyState.planned = false;
    applyState.key = '';
    $('#apply-plan').hidden = true;
    $('#apply-results').hidden = true;
    var box = $('#apply-confirm');
    if (box) { box.checked = false; }
    setApplyButtonEnabled();
    setHint('apply-hint', '输入已变化，请重新生成差异预览。', false);
  }

  function setApplyButtonEnabled() {
    var btn = $('[data-action="apply-exec"]');
    var box = $('#apply-confirm');
    if (btn) { btn.disabled = !(box && box.checked && applyState.planned); }
  }

  function renderApplySpecOptions() {
    var select = $('#apply-spec');
    if (!select) { return; }
    var keep = select.value;
    clear(select);
    var first = el('option', null, '（请选择适配器）');
    first.value = '';
    select.appendChild(first);
    state.adapters.forEach(function (a) {
      var opt = el('option', null, a.name + ' · ' + a.id + (a.detected ? '（本机已检测到）' : ''));
      opt.value = a.id;
      select.appendChild(opt);
    });
    if (keep) { select.value = keep; }
    if (!state.adapters.length) {
      setHint('apply-spec-hint', '还没有适配器列表：请先到「检测」页点一次「重新检测」。', true);
    } else {
      setHint('apply-spec-hint', '共 ' + state.adapters.length + ' 个适配器可用。', false);
    }
    updateSpecHint();
  }

  function updateSpecHint() {
    var id = $('#apply-spec').value;
    if (!id) { return; }
    var found = null;
    for (var i = 0; i < state.adapters.length; i++) {
      if (state.adapters[i].id === id) { found = state.adapters[i]; break; }
    }
    if (!found) { return; }
    setHint('apply-spec-hint',
      found.detected ? '该适配器在本机检测到，可直接应用。' : '注意：该适配器本机未检测到任何证据（仍可强制应用）。',
      !found.detected);
  }

  function renderApplyModelOptions() {
    var select = $('#apply-model');
    if (!select) { return; }
    var keep = select.value;
    clear(select);
    var auto = el('option', null, '（默认：网关第一个可用模型）');
    auto.value = '';
    select.appendChild(auto);
    var list = cache.models || [];
    list.forEach(function (m) {
      var label = m.id + (m.display_name && m.display_name !== m.id ? ' · ' + m.display_name : '');
      var opt = el('option', null, label);
      opt.value = m.id;
      select.appendChild(opt);
    });
    if (keep) { select.value = keep; }
    if (!list.length) {
      setHint('apply-model-hint', '读不到模型列表，可勾选「手动输入模型 id」直接填写。', true);
    }
  }

  function effectiveModel() {
    var manualOn = $('#apply-model-text-on');
    var manual = $('#apply-model-text');
    if (manualOn && manualOn.checked && manual && manual.value.trim()) { return manual.value.trim(); }
    return $('#apply-model').value;
  }

  function renderSteps(host, steps) {
    clear(host);
    if (!steps.length) {
      host.appendChild(el('li', 'hint', '后端没有返回任何步骤（可能该适配器不需要改动任何文件）。'));
      return;
    }
    steps.forEach(function (s) {
      var li = el('li', 'step-item');
      var meta = el('div', 'step-meta');
      meta.appendChild(el('span', 'step-kind', kindLabel(s.kind)));
      if (s.spec_id) { meta.appendChild(el('span', 'id-tag', s.spec_id)); }
      li.appendChild(meta);
      if (s.target) { li.appendChild(el('div', 'step-target', s.target)); }
      if (s.summary) { li.appendChild(el('div', 'step-summary', s.summary)); }
      host.appendChild(li);
    });
  }

  function renderDiff(pre, text) {
    clear(pre);
    if (!text) {
      pre.appendChild(el('span', 'diff-line meta',
        '（后端未返回差异文本：该计划可能只涉及环境变量提示，不写任何文件）'));
      return;
    }
    var lines = String(text).split(/\r\n|\r|\n/);
    lines.forEach(function (line) {
      var safe = maskText(line);
      var cls = 'diff-line';
      var head3 = safe.slice(0, 3);
      if (head3 === '+++' || head3 === '---') { cls += ' meta'; }
      else if (safe.slice(0, 2) === '@@') { cls += ' hunk'; }
      else if (safe.charAt(0) === '+') { cls += ' add'; }
      else if (safe.charAt(0) === '-') { cls += ' del'; }
      pre.appendChild(el('span', cls, safe));
    });
  }

  function buildPlanRequest() {
    var specID = $('#apply-spec').value;
    if (!specID) { return { error: '请先选择适配器。' }; }
    var model = effectiveModel();
    var keyInput = $('#apply-key');
    var key = keyInput && keyInput.value ? keyInput.value : '';
    if (keyInput) { keyInput.value = ''; } // 读走即清空，页面上不再留存
    applyState.specID = specID;
    applyState.model = model;
    applyState.key = key;
    var body = { spec_id: specID, model: model };
    if (key) { body.api_key = key; }
    return { body: body };
  }

  function submitPlan(event) {
    if (event) { event.preventDefault(); }
    var built = buildPlanRequest();
    if (built.error) {
      setHint('apply-hint', built.error, true);
      notify('warn', built.error);
      return;
    }
    setHint('apply-hint', '正在生成计划（不落盘）…', false);
    $('#apply-results').hidden = true;
    api('/api/plan', { method: 'POST', body: built.body }).then(function (data) {
      var steps = pickArray(data, ['steps', 'plan', 'items']);
      var diff = pickString(data, ['diff', 'diff_text']);
      var warning = pickString(data, ['warning']);
      renderSteps($('#apply-steps'), steps);
      renderDiff($('#apply-diff'), diff);
      $('#apply-plan').hidden = false;
      applyState.planned = true;
      var box = $('#apply-confirm');
      box.checked = false;
      setApplyButtonEnabled();
      if (warning) {
        setHint('apply-hint', '计划已生成（' + steps.length + ' 个步骤），但后端提示：' + warning, true);
        notify('warn', '计划已生成，但后端提示：' + warning);
      } else {
        setHint('apply-hint',
          '计划已生成：' + steps.length + ' 个步骤，尚未写入任何文件。核对差异后勾选确认即可写入。', false);
      }
      $('#apply-plan').scrollIntoView({ block: 'nearest' });
      return true;
    }, function (err) {
      applyState.planned = false;
      $('#apply-plan').hidden = true;
      setApplyButtonEnabled();
      setHint('apply-hint', '生成计划失败：' + errorText(err), true);
      notify('error', '生成计划失败：' + errorText(err));
    });
  }

  function executeApply() {
    if (!applyState.planned) {
      notify('warn', '还没有可执行的计划：请先生成差异预览。');
      return;
    }
    if (!$('#apply-confirm').checked) {
      notify('warn', '请先勾选「我已核对上述差异，确认写入」。');
      return;
    }
    if (!applyState.specID) {
      notify('warn', '适配器已丢失，请重新生成计划。');
      return;
    }
    var body = { spec_id: applyState.specID, model: applyState.model, confirm: true };
    if (applyState.key) { body.api_key = applyState.key; }
    setHint('apply-hint', '正在写入（会先备份原文件）…', false);
    api('/api/apply', { method: 'POST', body: body, timeout: REQUEST_TIMEOUT_MS }).then(function (data) {
      applyState.key = '';
      var results = pickArray(data, ['results', 'items', 'steps']);
      renderApplyResults(results);
      var failed = results.filter(function (r) { return r && r.result === 'failed'; }).length;
      if (failed) {
        notify('error', '写入完成，但有 ' + failed + ' 项失败，请看下方逐项结果。');
      } else {
        notify('ok', '写入完成：' + results.length + ' 项全部成功（原文件已备份）。');
      }
      setHint('apply-hint', '执行完成，原始文件已保留 <目标文件>.bak-<unix 时间戳> 备份。', false);
      loadState();
    }, function (err) {
      applyState.key = '';
      setHint('apply-hint', '写入失败：' + errorText(err), true);
      notify('error', '写入失败：' + errorText(err));
    });
  }

  function renderApplyResults(results) {
    var host = clear($('#apply-results-list'));
    $('#apply-results').hidden = false;
    if (!results.length) {
      host.appendChild(el('li', 'hint', '后端没有返回逐项结果。'));
      return;
    }
    results.forEach(function (r) {
      r = r || {};
      var ok = r.result === 'ok';
      var li = el('li', ok ? 'result-item ok' : 'result-item failed');
      li.appendChild(badge(ok ? 'pass' : 'fail', ok ? 'OK' : 'FAILED'));
      if (r.spec_id) { li.appendChild(el('span', 'id-tag', r.spec_id)); }
      if (r.target) { li.appendChild(el('span', 'result-target', r.target)); }
      if (r.message) { li.appendChild(el('span', 'result-message', r.message)); }
      host.appendChild(li);
    });
  }

  /* ══ 13. 体检页 ══════════════════════════════════════════════════════ */
  function normalizeStatus(status) {
    var s = String(status || '').toLowerCase();
    if (s === 'pass' || s === 'ok') { return 'pass'; }
    if (s === 'fail' || s === 'error') { return 'fail'; }
    if (s === 'warn' || s === 'warning') { return 'warn'; }
    return 'warn';
  }

  function loadDoctor() {
    setHint('doctor-summary', '正在体检…', false);
    clear($('#doctor-list'));
    return api('/api/doctor').then(function (data) {
      var items = pickArray(data, ['items', 'checks', 'results']);
      renderDoctor(items);
      return items;
    }, function (err) {
      setHint('doctor-summary', '体检失败：' + errorText(err), true);
      var host = clear($('#doctor-list'));
      host.appendChild(el('li', 'check-item fail',
        '无法完成体检：' + errorText(err)));
      notify('error', '体检失败：' + errorText(err));
    });
  }

  function renderDoctor(items) {
    var host = clear($('#doctor-list'));
    if (!items.length) {
      host.appendChild(el('li', 'check-item warn', '体检没有返回任何项目。'));
      setHint('doctor-summary', '0 项', false);
      return;
    }
    var counts = { pass: 0, warn: 0, fail: 0 };
    items.forEach(function (it) {
      it = it || {};
      var status = normalizeStatus(it.status);
      counts[status]++;
      var li = el('li', 'check-item ' + status);
      var nameRow = el('div', 'check-head');
      nameRow.appendChild(badge(status, status.toUpperCase()));
      nameRow.appendChild(el('span', 'check-name', it.name || '(未命名检查项)'));
      li.appendChild(nameRow);
      li.appendChild(el('p', 'check-detail', it.detail || '（无详情）'));
      host.appendChild(li);
    });
    setHint('doctor-summary',
      '共 ' + items.length + ' 项：PASS ' + counts.pass + ' / WARN ' + counts.warn + ' / FAIL ' + counts.fail, false);
    if (counts.fail > 0) {
      notify('error', '体检发现 ' + counts.fail + ' 项 FAIL，请看「体检」页逐项详情。');
    }
  }

  /* ══ 14. 适配器页 ════════════════════════════════════════════════════ */
  function loadSpecs(force) {
    if (cache.specs && !force) { renderSpecs(cache.specs); return Promise.resolve(cache.specs); }
    setHint('specs-hint', '正在读取适配器规格…', false);
    return api('/api/specs').then(function (data) {
      var list = pickArray(data, ['specs', 'adapters', 'items']);
      cache.specs = list;
      renderSpecs(list);
      setHint('specs-hint', '共 ' + list.length + ' 份规格（内置 + ~/.ximo-plugin/specs 下的自定义）。', false);
      return list;
    }, function (err) {
      setHint('specs-hint', '读取失败：' + errorText(err), true);
      notify('error', '读取适配器规格失败：' + errorText(err));
      throw err;
    });
  }

  function specFieldList(spec) {
    var box = el('div');
    var env = spec.env || {};
    var envRows = [];
    if (env.base_url) { envRows.push('base_url = ' + env.base_url); }
    if (env.api_key) { envRows.push('api_key = ' + env.api_key); }
    if (env.model) { envRows.push('model = ' + env.model); }
    if (Array.isArray(env.extra)) {
      env.extra.forEach(function (x) { envRows.push('extra = ' + x); });
    }
    if (envRows.length) {
      box.appendChild(el('p', 'spec-meta', '环境变量形态：'));
      var ul = el('ul', 'spec-fields');
      envRows.forEach(function (row) { ul.appendChild(el('li', null, row)); });
      box.appendChild(ul);
    }
    var files = Array.isArray(spec.files) ? spec.files : [];
    if (files.length) {
      box.appendChild(el('p', 'spec-meta', '配置文件形态（' + files.length + ' 个目标）：'));
      var fl = el('ul', 'spec-fields');
      files.forEach(function (f) {
        f = f || {};
        var fields = Array.isArray(f.fields) ? f.fields : [];
        var paths = fields.map(function (x) { return x && x.path ? x.path : '?'; }).join(', ');
        fl.appendChild(el('li', null,
          (f.path || '?') + '  [' + (f.format || '?') + (f.create ? ', create' : '') + ']  → ' + paths));
      });
      box.appendChild(fl);
    }
    return box;
  }

  function specCard(spec) {
    spec = spec || {};
    var card = el('article', 'card spec-card');
    var head = el('div', 'card-head');
    head.appendChild(el('h3', 'card-title', spec.name || '(未命名规格)'));
    head.appendChild(el('span', 'mono id-tag', spec.id || ''));
    var protocols = Array.isArray(spec.protocols) ? spec.protocols : [];
    protocols.forEach(function (p) { head.appendChild(el('span', 'tag', p)); });
    var actions = el('div', 'card-actions');
    var copyBtn = el('button', 'btn btn-quiet', '复制 JSON');
    copyBtn.type = 'button';
    copyBtn.setAttribute('data-copy-spec', spec.id || '');
    actions.appendChild(copyBtn);
    head.appendChild(actions);
    card.appendChild(head);

    if (spec.description) { card.appendChild(el('p', 'spec-meta', spec.description)); }
    if (spec.restart_note) { card.appendChild(el('p', 'spec-meta', '重启提示：' + spec.restart_note)); }

    var detect = spec.detect || {};
    var detectParts = [];
    if (Array.isArray(detect.binaries) && detect.binaries.length) { detectParts.push('可执行名 ' + detect.binaries.join(', ')); }
    if (Array.isArray(detect.paths) && detect.paths.length) { detectParts.push('路径 ' + detect.paths.join(', ')); }
    if (Array.isArray(detect.env) && detect.env.length) { detectParts.push('环境变量 ' + detect.env.join(', ')); }
    card.appendChild(el('p', 'spec-meta',
      '检测证据（any-of）：' + (detectParts.length ? detectParts.join('；') : '未声明（无法被自动检测到）')));

    card.appendChild(specFieldList(spec));

    var details = document.createElement('details');
    var summary = el('summary', null, '规格 JSON 全文（可整份复制去改）');
    details.appendChild(summary);
    var pre = document.createElement('pre');
    pre.className = 'spec-json';
    raw(pre, maskText(JSON.stringify(spec, null, 2)));
    details.appendChild(pre);
    card.appendChild(details);
    return card;
  }

  function renderSpecs(list) {
    var host = clear($('#spec-list'));
    if (!list.length) {
      host.appendChild(el('div', 'empty', '后端没有返回任何规格。'));
      return;
    }
    list.forEach(function (spec) { host.appendChild(specCard(spec)); });
  }

  function findSpec(id) {
    var list = cache.specs || [];
    for (var i = 0; i < list.length; i++) {
      if (list[i] && list[i].id === id) { return list[i]; }
    }
    return null;
  }

  /* ══ 15. 剪贴板 ══════════════════════════════════════════════════════ */
  function copyText(text, doneMsg) {
    function ok() { notify('ok', doneMsg); }
    function fail() { notify('warn', '复制失败，请手动选中文本复制。'); }
    function legacy() {
      try {
        var ta = document.createElement('textarea');
        ta.value = text;
        ta.setAttribute('readonly', 'readonly');
        ta.style.position = 'fixed';
        ta.style.top = '-1000px';
        document.body.appendChild(ta);
        ta.select();
        var done = document.execCommand('copy');
        document.body.removeChild(ta);
        if (done) { ok(); } else { fail(); }
      } catch (e) { fail(); }
    }
    if (navigator.clipboard && navigator.clipboard.writeText) {
      navigator.clipboard.writeText(text).then(ok, legacy);
      return;
    }
    legacy();
  }

  /* ══ 16. 页面加载器与事件接线 ════════════════════════════════════════ */
  var pageLoaders = {
    detect: runDetect,
    connect: function () { renderConnect(); return loadState(); },
    models: function () { return loadModels(false); },
    apply: function () {
      renderApplySpecOptions();
      // 直接打开 #apply 时 /api/state 可能还没回来：先补齐适配器列表，再拉模型。
      var wait = state.adapters.length ? Promise.resolve() : loadState().then(null, function () {});
      return wait.then(function () {
        renderApplySpecOptions();
        return loadModels(false);
      });
    },
    doctor: loadDoctor,
    adapters: loadSpecs
  };

  function wireEvents() {
    document.addEventListener('click', function (event) {
      var target = event.target;
      var elWithAction = target && target.closest ? target.closest('[data-action]') : null;
      if (elWithAction) {
        var action = elWithAction.getAttribute('data-action');
        if (action === 'detect-refresh') { runDetect(); return; }
        if (action === 'connect-refresh') { loadState().then(function () { notify('ok', '状态已刷新。'); }); return; }
        if (action === 'models-refresh') { loadModels(true).then(null, function () {}); return; }
        if (action === 'apply-clear') {
          applyState.specID = ''; applyState.model = ''; applyState.key = ''; applyState.planned = false;
          $('#apply-form').reset();
          $('#apply-plan').hidden = true;
          $('#apply-results').hidden = true;
          $('#apply-model-text').hidden = true;
          setApplyButtonEnabled();
          setHint('apply-hint', '已清空。', false);
          return;
        }
        if (action === 'apply-exec') { executeApply(); return; }
        if (action === 'doctor-refresh') { loadDoctor(); return; }
        if (action === 'specs-refresh') { loadSpecs(true).then(null, function () {}); return; }
        if (action === 'login-start') { startLogin(); return; }
        if (action === 'login-poll') { pollLogin(); return; }
        if (action === 'login-stop') { stopLogin('已停止轮询。'); return; }
        if (action === 'copy-code') {
          if (!login.code) { notify('warn', '还没有用户码。'); return; }
          copyText(login.code, '用户码已复制到剪贴板。');
          return;
        }
        return;
      }
      var copySpec = target && target.closest ? target.closest('[data-copy-spec]') : null;
      if (copySpec) {
        var spec = findSpec(copySpec.getAttribute('data-copy-spec'));
        if (!spec) { notify('warn', '找不到对应规格，请先刷新规格。'); return; }
        copyText(JSON.stringify(spec, null, 2), '规格 JSON 已复制，可直接粘贴到 ~/.ximo-plugin/specs/ 下修改。');
      }
    });

    window.addEventListener('hashchange', function () {
      clearNotify();
      showPage(currentPage(), false);
    });

    var form = $('#apply-form');
    if (form) { form.addEventListener('submit', submitPlan); }

    // 任何输入变化都会让已生成的计划失效（避免用旧差异写入新配置）
    ['#apply-spec', '#apply-model', '#apply-model-text', '#apply-key'].forEach(function (sel) {
      var node = $(sel);
      if (!node) { return; }
      node.addEventListener('input', invalidatePlan);
      node.addEventListener('change', invalidatePlan);
    });

    var manualToggle = $('#apply-model-text-on');
    if (manualToggle) {
      manualToggle.addEventListener('change', function () {
        var textInput = $('#apply-model-text');
        textInput.hidden = !manualToggle.checked;
        if (manualToggle.checked) { textInput.focus(); }
      });
    }

    var confirmBox = $('#apply-confirm');
    if (confirmBox) { confirmBox.addEventListener('change', setApplyButtonEnabled); }
  }

  /* ══ 17. 启动 ════════════════════════════════════════════════════════ */
  function boot() {
    var heading = $('.masthead-title');
    if (heading) { heading.setAttribute('tabindex', '-1'); }

    wireEvents();
    renderStatusChips();
    renderConnect();
    showPage(currentPage(), true);

    if (!uiToken) {
      notify('error',
        '当前 URL 缺少会话令牌：请使用 `ximo-plugin ui` 启动时打印的完整地址（形如本机回环地址 + ' +
        '/?t= 令牌参数）打开本页面；否则所有 /api/* 都会返回 401。');
      setHint('detect-hint', '未提供会话令牌，已跳过自动加载。', true);
      return;
    }

    loadState().then(null, function (err) {
      notify('error', '读取 /api/state 失败：' + errorText(err));
    });
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', boot);
  } else {
    boot();
  }
})();
