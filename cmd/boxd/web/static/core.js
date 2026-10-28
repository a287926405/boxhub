/* BoxHub 控制台 — 核心:工具、状态、API、登录、布局导航、仪表盘
 *
 * 拆成 core.js / pages.js 两个文件是为了可维护性:
 *   core.js  —— 基础设施 + 仪表盘
 *   pages.js —— 各功能页面
 * 两者共享全局的 state / h / api 等符号(都是普通脚本,非模块)。
 */
'use strict';

// ---------------------------------------------------------------------------
// 工具
// ---------------------------------------------------------------------------

const $ = (s, r = document) => r.querySelector(s);
const $$ = (s, r = document) => Array.from(r.querySelectorAll(s));

const SVG_TAGS = new Set(['svg', 'path', 'circle', 'rect', 'line', 'g', 'polygon', 'polyline', 'text']);

function h(tag, attrs, ...kids) {
  // SVG 子元素必须走 SVG 命名空间创建:document.createElement('svg') 生成的
  // 是普通 HTML 元素,浏览器根本不渲染(之前站点图标"不显示"就是这个原因)。
  const el = SVG_TAGS.has(tag)
    ? document.createElementNS('http://www.w3.org/2000/svg', tag)
    : document.createElement(tag);
  if (attrs) {
    for (const [k, v] of Object.entries(attrs)) {
      if (v === null || v === undefined || v === false) continue;
      if (k === 'class') {
        // 统一走 setAttribute:SVGElement.className 是只读的
        // (SVGAnimatedString),直接赋值会抛 TypeError 打断整段渲染。
        el.setAttribute('class', v);
      }
      else if (k === 'html') el.innerHTML = v;
      else if (k.startsWith('on') && typeof v === 'function') el.addEventListener(k.slice(2), v);
      else if (k === 'style' && typeof v === 'object') Object.assign(el.style, v);
      else el.setAttribute(k, v === true ? '' : v);
    }
  }
  for (const c of kids.flat(9)) {
    if (c === null || c === undefined || c === false || c === '') continue;
    el.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
  return el;
}

const clear = (el) => { while (el.firstChild) el.removeChild(el.firstChild); return el; };

function toast(msg, type = 'info', ms = 3200) {
  let wrap = $('.toasts');
  if (!wrap) { wrap = h('div', { class: 'toasts' }); document.body.append(wrap); }
  const t = h('div', { class: 'toast ' + type }, msg);
  wrap.append(t);
  setTimeout(() => {
    t.style.opacity = '0'; t.style.transition = 'opacity .3s';
    setTimeout(() => t.remove(), 320);
  }, ms);
}

async function api(method, path, body) {
  const opt = { method, headers: {} };
  if (body !== undefined) {
    opt.headers['Content-Type'] = 'application/json';
    opt.body = JSON.stringify(body);
  }
  const res = await fetch(path, opt);
  let data = null;
  try { data = await res.json(); } catch (e) { /* 无响应体 */ }
  if (res.status === 401) {
    state.authed = false;
    renderLogin();
    throw new Error('登录已过期,请重新登录');
  }
  if (!res.ok || (data && data.ok === false)) {
    throw new Error((data && data.error) || ('请求失败 HTTP ' + res.status));
  }
  return data;
}

const fmtBytes = (n) => {
  if (!n) return '0 B';
  const u = ['B', 'KB', 'MB', 'GB', 'TB'];
  let i = 0, v = n;
  while (v >= 1024 && i < u.length - 1) { v /= 1024; i++; }
  return (i === 0 ? v : v.toFixed(v >= 100 ? 0 : 1)) + ' ' + u[i];
};
const fmtTime = (s) => {
  if (!s) return '—';
  const d = new Date(s);
  if (isNaN(d) || d.getFullYear() < 2000) return '—';
  const p = (x) => String(x).padStart(2, '0');
  return d.getFullYear() + '-' + p(d.getMonth() + 1) + '-' + p(d.getDate()) + ' ' +
    p(d.getHours()) + ':' + p(d.getMinutes()) + ':' + p(d.getSeconds());
};
const relTime = (s) => {
  if (!s) return '从未';
  const diff = (Date.now() - new Date(s)) / 1000;
  if (diff < 60) return '刚刚';
  if (diff < 3600) return Math.floor(diff / 60) + ' 分钟前';
  if (diff < 86400) return Math.floor(diff / 3600) + ' 小时前';
  return Math.floor(diff / 86400) + ' 天前';
};
const pct = (v) => (v > 0 ? Number(v).toFixed(1) + '%' : '0%');

// ---------------------------------------------------------------------------
// 状态
// ---------------------------------------------------------------------------

const state = {
  authed: false,
  needsSetup: false,
  page: 'dashboard',
  dashboard: null,
  config: null,
  filters: [],
  chains: [],
  frontOptions: [],
  timer: null,
};

// ---------------------------------------------------------------------------
// 登录
// ---------------------------------------------------------------------------

function renderLogin() {
  stopRefresh();
  const app = clear($('#app'));
  const setup = state.needsSetup;
  const pw = h('input', { type: 'password', placeholder: '至少 6 位' });
  const pw2 = h('input', { type: 'password', placeholder: '再输入一次' });
  const msg = h('div', { class: 'small', style: { color: 'var(--red)', minHeight: '20px' } });
  const btn = h('button', { class: 'primary', style: { width: '100%', marginTop: '6px' } },
    setup ? '设置并进入' : '登录');

  const submit = async () => {
    msg.textContent = '';
    btn.disabled = true;
    try {
      if (setup) {
        if (pw.value.length < 6) throw new Error('密码至少 6 位');
        if (pw.value !== pw2.value) throw new Error('两次输入的密码不一致');
      }
      await api('POST', '/api/login', { password: pw.value });
      await boot();
    } catch (e) {
      msg.textContent = e.message;
      btn.disabled = false;
    }
  };
  btn.addEventListener('click', submit);
  [pw, pw2].forEach((i) => i.addEventListener('keydown', (e) => { if (e.key === 'Enter') submit(); }));

  app.append(
    h('div', { class: 'login-wrap' },
      h('div', { class: 'login-box' },
        h('div', { class: 'login-brand' },
          brandLogo(52),
          h('h1', {}, setup ? '初始化 BoxHub' : 'BoxHub'),
          h('p', {}, setup ? '首次使用,请设置访问密码' : '请输入访问密码')),
        h('div', { class: 'card' },
          h('div', { class: 'field' }, h('label', {}, '密码'), pw),
          setup ? h('div', { class: 'field' }, h('label', {}, '确认密码'), pw2) : null,
          msg, btn))));
  setTimeout(() => pw.focus(), 60);
}

async function doLogout() {
  try { await api('POST', '/api/logout'); } catch (e) { /* 忽略 */ }
  location.reload();
}

// ---------------------------------------------------------------------------
// 导航
// ---------------------------------------------------------------------------

const NAV = [
  { group: '监控', items: [
    { id: 'dashboard', name: '仪表盘', ico: '◉' },
    { id: 'dns', name: 'DNS 监控', ico: '◎' },
  ] },
  { group: '配置', items: [
    { id: 'nodes', name: '节点', ico: '▤' },
    { id: 'filters', name: '筛选', ico: '⛃' },
    { id: 'rules', name: '规则', ico: '⇉' },
    { id: 'chains', name: '链式代理', ico: '⛓' },
    { id: 'advanced', name: '高级配置', ico: '⚒' },
  ] },
  { group: '系统', items: [
    { id: 'kernel-ui', name: '内核面板', ico: '▦' },
    { id: 'logs', name: '日志', ico: '☰' },
    { id: 'settings', name: '设置', ico: '⚙' },
  ] },
];
const PAGES = NAV.flatMap((g) => g.items);

function navTo(page) {
  // 渲染只由 hashchange 驱动。
  // 之前这里先改了 state.page 再改 hash,异步触发的 hashchange 处理器
  // 看到 p === state.page 以为已经渲染过,直接跳过 —— 表现为"要点两次才切换"
  // (第一次只改了状态没画面变化,第二次 hash 已相等走 else 分支才渲染)。
  if (location.hash === '#' + page) {
    renderPage(); // 同页刷新
    return;
  }
  location.hash = page;
}

function renderShell() {
  const app = clear($('#app'));
  const main = h('main', { class: 'main' });
  const sidebar = h('aside', { class: 'sidebar' },
    h('div', { class: 'brand' },
      h('div', { class: 'logo' }, brandLogo(26)),
      h('div', {}, h('div', {}, 'BoxHub'), h('div', { class: 'pro' }, 'OPEN'))),
    ...NAV.map((g) => h('div', { class: 'nav-group' },
      h('div', { class: 'nav-group-title' }, g.group),
      ...g.items.map((it) => h('div', {
        class: 'nav-item', 'data-page': it.id, onclick: () => navTo(it.id),
      }, h('span', { class: 'ico' }, it.ico), h('span', {}, it.name))))),
    h('div', { class: 'sidebar-footer' },
      h('div', { class: 'nav-item', onclick: openKernelUINewTab, title: '在新窗口打开内核面板' },
        h('span', { class: 'ico' }, '↗'), h('span', {}, '新窗口打开')),
      h('div', { class: 'nav-item', onclick: showHelp }, h('span', { class: 'ico' }, '?'), h('span', {}, '帮助 / 关于')),
      h('div', { class: 'nav-item', onclick: doLogout }, h('span', { class: 'ico' }, '⏻'), h('span', {}, '退出登录'))),
  );
  app.append(h('div', { class: 'app' }, sidebar, main));
  return main;
}

async function renderPage() {
  let main = $('.main');
  if (!main) main = renderShell();
  clear(main);
  $$('.nav-item[data-page]').forEach((el) => el.classList.toggle('active', el.dataset.page === state.page));
  stopRefresh();
  const map = {
    dashboard: renderDashboard, dns: renderDNS, nodes: renderNodes, filters: renderFilters,
    rules: renderRules, chains: renderChains, advanced: renderAdvanced,
    'kernel-ui': renderKernelUI, logs: renderLogs, settings: renderSettings,
  };
  const fn = map[state.page] || renderDashboard;
  try {
    await fn(main);
  } catch (e) {
    main.append(h('div', { class: 'card' }, h('span', { class: 'badge err' }, e.message)));
  }
}

function stopRefresh() {
  if (state.timer) { clearInterval(state.timer); state.timer = null; }
}

function startRefresh(fn, ms) {
  stopRefresh();
  state.timer = setInterval(() => {
    if (state.page === 'dashboard' || state.page === 'dns') fn();
  }, ms);
}

// ---------------------------------------------------------------------------
// 仪表盘
// ---------------------------------------------------------------------------

function statCard(ico, cls, label, value) {
  return h('div', { class: 'stat' },
    h('div', { class: 'ico ' + cls }, ico),
    h('div', { style: { minWidth: 0, overflow: 'hidden' } },
      h('div', { class: 'lbl' }, label),
      h('div', { class: 'val', style: { overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' } }, value)));
}

function kv(k, v) {
  return h('div', { class: 'kv' }, h('span', { class: 'k' }, k), h('span', { class: 'v' }, v));
}

function barRow(label, value, color) {
  value = Number(value) || 0;
  return h('div', { style: { marginBottom: '10px' } },
    h('div', { class: 'row' },
      h('span', { class: 'small', style: { color: 'var(--text-dim)' } }, label),
      h('span', { class: 'spacer' }),
      h('span', { class: 'small', style: { fontWeight: '600' } }, pct(value))),
    h('div', { class: 'bar' }, h('i', { class: color, style: { width: Math.min(100, value) + '%' } })));
}

async function renderDashboard(root) {
  root.append(h('h1', { class: 'page-title' }, '仪表盘'));
  const speedBox = h('div');
  const body = h('div');
  root.append(speedBox, body);
  mountSpeedtest(speedBox);

  const draw = async () => {
    const d = await api('GET', '/api/dashboard');
    state.dashboard = d;
    clear(body);
    const s = d.system || {}, k = d.kernel || {}, c = d.counts || {}, t = d.traffic || {};

    body.append(h('div', { class: 'grid g4' },
      statCard('▣', 'i-blue', '主机名', s.hostname || '—'),
      statCard('◈', 'i-purple', 'CPU 使用率', pct(s.cpu_percent)),
      statCard('▤', 'i-green', '内存使用率', pct(s.mem_percent)),
      statCard('⇅', 'i-orange', '网络传输', '↑' + fmtBytes(t.rate_up) + ' ↓' + fmtBytes(t.rate_down)),
      statCard('▦', 'i-blue', '总节点数', String(c.nodes || 0)),
      statCard('▥', 'i-purple', '订阅数', String(c.subscriptions || 0)),
      statCard('⛃', 'i-green', '筛选数', String(c.filters || 0)),
      statCard('⇉', 'i-orange', '规则数', String(c.rules || 0))));

    const dev = h('div', { class: 'card' },
      h('div', { class: 'card-head' },
        h('div', { class: 'ico i-blue' }, '▣'),
        h('div', {}, h('div', { class: 'card-title' }, '设备信息'),
          h('div', { class: 'card-sub' }, (s.os || '') + ' ' + (s.arch || '')))),
      kv('主机名', s.hostname || '—'), kv('IP 地址', s.ip || '—'),
      kv('CPU', s.cpu_model || '—'), kv('内核', s.kernel || '—'),
      s.container ? kv('运行环境', '容器 (' + s.container + ')') : null,
      s.tun_available ? kv('TUN', h('span', { class: 'badge ok' }, '可用'))
        : kv('TUN', h('span', { class: 'badge warn' }, '不可用')));

    const hw = h('div', { class: 'card' },
      h('div', { class: 'card-head' },
        h('div', { class: 'ico i-purple' }, '▤'),
        h('div', {}, h('div', { class: 'card-title' }, '硬件信息'),
          h('div', { class: 'card-sub' }, '内存 ' + fmtBytes(s.mem_total)))),
      barRow('CPU 使用率', s.cpu_percent, ''),
      barRow('内存使用率', s.mem_percent, s.mem_percent > 80 ? 'red' : 'green'),
      barRow('磁盘', s.disk_percent, s.disk_percent > 85 ? 'red' : 'orange'),
      kv('内存用量', fmtBytes(s.mem_used) + ' / ' + fmtBytes(s.mem_total)),
      kv('磁盘用量', fmtBytes(s.disk_used) + ' / ' + fmtBytes(s.disk_total)),
      kv('负载', (s.load_avg || [0, 0, 0]).map((x) => Number(x).toFixed(2)).join(' / ')));

    const kc = h('div', { class: 'card' },
      h('div', { class: 'card-head' },
        h('div', { class: 'ico i-green' }, '◈'),
        h('div', {}, h('div', { class: 'card-title' }, '内核'),
          h('div', { class: 'card-sub' }, k.version || '—'))),
      kv('运行状态', k.running
        ? h('span', { class: 'badge ok' }, h('span', { class: 'dot' }), '运行中')
        : h('span', { class: 'badge err' }, h('span', { class: 'dot' }), '未运行')),
      kv('启动时间', fmtTime(k.started_at)),
      kv('重启次数', String(k.restarts || 0)),
      kv('活跃连接', String(t.connections || 0)),
      h('div', { class: 'row', style: { marginTop: '12px' } },
        h('button', { class: 'sm', onclick: async (e) => {
          const b = e.currentTarget; b.disabled = true;
          try {
            await api('POST', '/api/kernel/' + (k.running ? 'stop' : 'start'));
            toast(k.running ? '内核已停止' : '内核已启动', 'ok');
            setTimeout(draw, 900);
          } catch (err) { toast(err.message, 'err', 6000); }
          b.disabled = false;
        } }, k.running ? '停止服务' : '启动服务'),
        h('button', { class: 'sm', onclick: async () => {
          try { await api('POST', '/api/kernel/restart'); toast('内核已重启', 'ok'); setTimeout(draw, 1500); }
          catch (err) { toast(err.message, 'err', 6000); }
        } }, '重启服务')));
    if (k.last_error) {
      kc.append(h('div', { class: 'small', style: { color: 'var(--red)', marginTop: '10px', wordBreak: 'break-all' } }, k.last_error));
    }

    body.append(h('div', { class: 'grid g3' }, dev, hw, kc));

    // 筛选统计
    const stats = d.filter_stats || [];
    const fcard = h('div', { class: 'card' },
      h('div', { class: 'card-head' },
        h('div', { class: 'ico i-blue' }, '⛃'),
        h('div', {}, h('div', { class: 'card-title' }, '筛选节点统计'),
          h('div', { class: 'card-sub' }, '筛选数: ' + stats.length)),
        h('div', { class: 'spacer' }),
        h('button', { class: 'sm', onclick: () => navTo('filters') }, '管理筛选')));
    if (!stats.length) {
      fcard.append(h('div', { class: 'empty' },
        h('div', { class: 'icon' }, '⛃'), h('div', {}, '还没有筛选'),
        h('div', { class: 'small', style: { marginTop: '8px' } }, '筛选可按地区/关键词自动归类订阅节点'),
        h('div', { style: { marginTop: '14px' } },
          h('button', { class: 'primary sm', onclick: autoCreateFilters }, '按地区自动生成'))));
    } else {
      fcard.append(h('div', { class: 'filter-grid' },
        ...stats.map((f) => h('div', { class: 'filter-item', onclick: () => navTo('filters') },
          h('span', { class: 'name' }, fDisplay(f)),
          h('span', { class: 'cnt' }, f.count + ' 个节点')))));
    }
    body.append(fcard);
    body.append(trafficCard());
  };

  await draw();
  startRefresh(draw, 5000);
}

async function autoCreateFilters() {
  try {
    const r = await api('POST', '/api/filters/auto');
    if (r.count > 0) toast('已生成 ' + r.count + ' 个地区筛选', 'ok');
    else toast('没有可生成的地区(可能已有全部地区筛选)', 'warn');
    renderPage();
  } catch (e) { toast(e.message, 'err', 6000); }
}

// ---------------------------------------------------------------------------
// 启动
// ---------------------------------------------------------------------------

// 清剿本域历史 Service Worker:面板(PWA)旧 SW 会用失效缓存劫持 /ui,
// 升级/换面板文件后表现为"内核面板打不开"(实测 curl 200 而浏览器 404)。
function purgeServiceWorkers() {
  try {
    if (!('serviceWorker' in navigator)) return;
    navigator.serviceWorker.getRegistrations().then((rs) => {
      rs.forEach((r) => { try { r.unregister(); } catch (e) {} });
      if (window.caches && caches.keys) {
        caches.keys().then((ks) => ks.forEach((k) => { try { caches.delete(k); } catch (e) {} }));
      }
    });
  } catch (e) { /* 无 SW 环境 */ }
}

async function boot() {
  purgeServiceWorkers();
  try {
    const s = await api('GET', '/api/session');
    state.authed = !!s.logged_in;
    state.needsSetup = !!s.needs_setup;
  } catch (e) {
    state.authed = false;
  }
  if (!state.authed) { renderLogin(); return; }
  const hash = location.hash.replace('#', '');
  if (hash && PAGES.some((p) => p.id === hash)) state.page = hash;
  if (!hash) {
    try { history.replaceState(null, '', '#dashboard'); } catch (e) { /* 忽略 */ }
  }
  renderPage();
}

window.addEventListener('hashchange', () => {
  const p = location.hash.replace('#', '');
  if (p && PAGES.some((x) => x.id === p) && p !== state.page) {
    state.page = p;
    renderPage();
  }
});

document.addEventListener('DOMContentLoaded', boot);

// 品牌 logo:白天/黑夜两套图,CSS 按 prefers-color-scheme 切换显示。
// (logo 文件内嵌在 boxd 里,离线可用,与面板主题机制同源)
function brandLogo(size) {
  return h('span', { class: 'brand-logo', style: { width: size + 'px', height: size + 'px' } },
    // 不带 ?v= 查询:__V__ 占位符只在 index.html 里被替换,JS 里不生效。
    // 图片未设 immutable,浏览器会按 Last-Modified 再验证,升级后能拿到新图。
    h('img', { class: 'lg-light', src: '/logo.png', alt: 'BoxHub', width: size, height: size }),
    h('img', { class: 'lg-dark', src: '/logo-dark.png', alt: 'BoxHub', width: size, height: size }));
}

// ---------------------------------------------------------------------------
// 测速条(对齐 Open-Box 面板概览):常驻仪表盘顶部
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// 站点图标(内置 SVG path,离线可用;颜色取品牌色)
// ---------------------------------------------------------------------------

const SITE_ICONS = {
  '百度': { c: '#2932E1', p: 'M12 2C7.5 2 4.4 5.6 4 10c-.3 3.7 1.6 6.6 4.6 7.5-1 .6-2.6 1.3-3.6 2.3-.7.7-.6 1.6.1 2 .9.5 3.6.4 5.4-.7 1.7-1 3-2.7 3.6-4.6 1.9-.2 4.4-.9 4.4-3.5 0-1.6-1.3-2.1-2.4-2.2.4-1.5.3-2.8-.4-3.9C16.5 13 15 13 14 13.6c-.3-1.7.6-3.5.9-4.5.3-1-.4-1.6-1-1.6-1.2 0-2.6 1.3-2.9 4.2-.4-.1-.8-.1-1.1 0-.2-2.5-.9-4.5 0-5.7' },
  'Google': { c: '#4285F4', p: 'M21.6 12.2c0-.7-.1-1.4-.2-2H12v4h5.4a4.6 4.6 0 0 1-2 3v2.4h3.2c1.9-1.7 2.9-4.3 2.9-7.4z|M12 22c2.7 0 5-.9 6.6-2.4l-3.2-2.4c-.9.6-2 .9-3.4.9a6 6 0 0 1-5.7-4.1H3v2.5A10 10 0 0 0 12 22z|M6.3 14a6 6 0 0 1 0-3.9V7.5H3a10 10 0 0 0 0 9l3.3-2.5z|M12 6c1.5 0 2.8.5 3.8 1.5L18.7 5A10 10 0 0 0 3 7.5l3.3 2.6A6 6 0 0 1 12 6z' },
  'OpenAI': { c: '#10a37f', p: 'M19.1 12.9a3.2 3.2 0 0 0 .3-2.4 5 5 0 0 0-2.2-3.1 3.2 3.2 0 0 0-3.1-4.3 5 5 0 0 0-3.6 1.8 3.2 3.2 0 0 0-5.4 3.5 5 5 0 0 0-.8 6 3.2 3.2 0 0 0 2.3 5.6 5 5 0 0 0 4.6 2.7 3.2 3.2 0 0 0 5.4-1.9 5 5 0 0 0 2.6-4.5 3.2 3.2 0 0 0 .9-3.4zm-6.9 6.5a2 2 0 0 1-1.3-1.2l-.1-.2h-.2a2 2 0 0 1-2.4-.6l-.1-.2v-.2h-1.5a2 2 0 0 1-1.6-1.9v-.2l-.1-.1a2 2 0 0 1 .7-2.9v-.2a2 2 0 0 1 .4-2.5l.2-.1v-.7a2 2 0 0 1 2-1.9l.3-.2a2 2 0 0 1 1.6-1.3h.3a2 2 0 0 1 1.9-.7h.2a2 2 0 0 1 1.8.9l.2.2a2 2 0 0 1 1.5.5l.1.2a2 2 0 0 1 1.9 1.2 2 2 0 0 1 .8 1.4v.2a2 2 0 0 1-.1.6l.1.2a2 2 0 0 1 .5 1.6l-.1.1a2 2 0 0 1-1.2 1.7v1.5a2 2 0 0 1-1.2 1.4l-.2.1a2 2 0 0 1-1 1l-.1.1a2 2 0 0 1-1.6.4h-.1a2 2 0 0 1-1.9.5z' },
  'Telegram': { c: '#29A9EB', p: 'M21.9 4.6 18.8 19.7c-.2 1-.9 1.3-1.7.8l-4.8-3.5-2.3 2.2c-.3.3-.5.5-.9.5l.3-4.8L18.2 6c.4-.3-.1-.5-.6-.2L7.1 12 2.4 10.5c-1-.3-1-.9.2-1.4L20.6 3.2c.8-.3 1.5.2 1.3 1.4z' },
};

function siteIcon(name) {
  const ic = SITE_ICONS[name];
  if (!ic) return h('span', { class: 'site-ico site-ico-default' }, '◎');
  // 多段 path 用 | 分隔(Google 四色)
  const paths = ic.p.split('|').map((d, i) => h('path', {
    d, fill: ic.c,
  }));
  return h('svg', { class: 'site-ico', viewBox: '0 0 24 24', width: 18, height: 18 }, ...paths);
}


let speedAuto = localStorage.getItem('boxhub.speedAuto') !== '0';
let speedTimer = null;

function mountSpeedtest(box) {
  const toggle = h('input', {
    type: 'checkbox', class: 'switch', checked: speedAuto,
    onchange: () => { speedAuto = toggle.checked; localStorage.setItem('boxhub.speedAuto', speedAuto ? '1' : '0'); runSpeed(); },
  });
  const items = h('div', { class: 'speed-items' });
  const manual = h('button', { class: 'speed-refresh', onclick: () => runSpeed(), title: '立即测速', 'aria-label': '立即测速' },
    h('svg', { viewBox: '0 0 24 24', width: 20, height: 20 },
      h('path', { d: 'M17.65 6.35A8 8 0 1 0 20 12h-2a6 6 0 1 1-1.76-4.24L13 11h7V4l-2.35 2.35z', fill: 'currentColor' })));
  box.append(h('div', { class: 'card speed-bar' },
    h('div', { class: 'row', style: { gap: '10px', marginBottom: '10px' } },
      h('span', { class: 'card-title', style: { fontSize: '13.5px' } }, '面板概览'),
      h('label', { style: { display: 'flex', gap: '6px', alignItems: 'center', cursor: 'pointer' } },
        toggle, h('span', { class: 'small dim' }, '打开概览自动测速')),
      h('span', { class: 'spacer' }), manual),
    items));
  runSpeed();
}

let speedTargets = null;

async function runSpeed() {
  const items = document.querySelector('.speed-items');
  if (!items) return;
  if (speedTimer) { clearTimeout(speedTimer); speedTimer = null; }
  // 目标名单很小(纯内存),拿不到就用内置四项兜底
  if (!speedTargets) {
    try {
      speedTargets = (await api('GET', '/api/speedtest?action=list')).targets || [];
    } catch (e) { /* 忽略,下面用默认 */ }
    if (!speedTargets.length) {
      // 与后端 speedTestTargets 保持一致的兜底名单
      speedTargets = [
        { name: '百度', url: 'https://www.baidu.com' },
        { name: 'Google', url: 'https://www.google.com/generate_204' },
        { name: 'OpenAI', url: 'https://api.openai.com/v1/models' },
        { name: 'Telegram', url: 'https://t.me/telegram' },
      ];
    }
  }
  // 拉一次服务内存历史(boxd 重启则用本地会话历史即可)
  try {
    const h = (await api('GET', '/api/speedtest?action=history')).history || {};
    for (const k of Object.keys(h)) {
      if (h[k].length > (speedLocalHist[k] || []).length) {
        speedLocalHist[k] = h[k].map((x) => ({ at: x.at, delay_ms: x.delay_ms, chain: x.chain || '' }));
      }
    }
  } catch (e) { /* 无历史也照常测 */ }
  // 先画"测速中"骨架:之前是等四个站点全部返回才渲染,
  // 打开页面前几秒整条测速区空白,像没加载出来。
  const results = speedTargets.map(() => null);
  const paint = () => {
    renderSpeed(items, speedTargets.map((t, i) =>
      results[i] || { name: t.name, delay_ms: 0, pending: true }));
  };
  paint();
  speedTargets.forEach((t, i) => {
    const q = '?url=' + encodeURIComponent(t.url) + '&name=' + encodeURIComponent(t.name);
    api('GET', '/api/speedtest' + q)
      .then((r) => {
        const one = (r.results || [])[0];
        results[i] = one || { name: t.name, delay_ms: -1, error: '无结果' };
      })
      .catch((e) => { results[i] = { name: t.name, delay_ms: -1, error: e.message }; })
      .then(() => {
        const rr = results[i];
        if (rr && !rr.pending) pushLocalHist(t.name, rr.delay_ms > 0 ? rr.delay_ms : -1, null, rr.chain);
        paint();
      });
  });
  if (speedAuto) speedTimer = setTimeout(() => {
    if (state.page === 'dashboard') runSpeed();
  }, 20000);
}

function renderSpeed(box, results) {
  box.replaceChildren(...results.map((t) => {
    const pending = t.pending;
    const ok = t.delay_ms > 0;
    const cls = pending ? 'pending' : !ok ? 'err' : speedCls(t.delay_ms);
    const el = h('div', { class: 'speed-item ' + cls, name: t.name },
      h('div', { class: 'row', style: { gap: '7px' } },
        siteIcon(t.name),
        h('span', { style: { fontWeight: '600', fontSize: '13px' } }, t.name),
        h('span', { class: 'spacer' }),
        h('span', { class: 'speed-val' }, pending ? '测速中…' : ok ? t.delay_ms + ' ms' : '—')));
    // 历史柱直接内联在卡片里(Open-Box 风格):悬停单根柱看该时刻延时与线路
    el.append(speedBars(t.name, ok ? t.delay_ms : null));
    return el;
  }));
}

// speedCls 测速颜色阈值:500ms 以下绿,1000ms 以下黄,以上红。
// (卡片数字、虚线柱、历史小柱、浮层全部共用,避免多处写死后不一致)
function speedCls(ms) {
  if (!(ms > 0)) return 'err'; // 无延迟/失败:灰
  if (ms < 500) return 'good';
  if (ms < 1000) return 'mid';
  return 'bad';
}

// speedBars 渲染一张卡的历史柱状图。悬停浮层按"柱"独立触发,
// 显示该样本的时间、延时、当时所走线路。
function speedBars(name, currentDelay) {
  const samples = (speedLocalHist[name] || []).slice(-36);
  const wrap = h('div', { class: 'tip-bars inline' });
  if (!samples.length) {
    // 没历史时保留一根"当前"占位,避免空框
    if (currentDelay && currentDelay > 0) {
      samples.push({ at: Date.now() / 1000, delay_ms: currentDelay, chain: '' });
    } else {
      return wrap;
    }
  }
  for (const sp of samples) {
    const fail = !(sp.delay_ms > 0);
    const px = fail ? 3 : Math.max(4, Math.min(26, sp.delay_ms / 14));
    const bar = h('span', {
      class: 'tip-bar ' + (fail ? 'fail' : speedCls(sp.delay_ms)),
      style: { height: px + 'px' },
    });
    attachBarTip(bar, name, sp);
    wrap.append(bar);
  }
  return wrap;
}

let tipEl = null, tipTimer = null;

function hideSpeedTip() {
  if (tipTimer) { clearTimeout(tipTimer); tipTimer = null; }
  if (tipEl) { tipEl.remove(); tipEl = null; }
}

function attachBarTip(bar, name, sp) {
  bar.addEventListener('mouseenter', (e) => {
    if (tipTimer) clearTimeout(tipTimer);
    tipTimer = setTimeout(() => {
      hideSpeedTip();
      const lines = [];
      const chain = (sp.chain || '').split(' → ');
      // 展示:线路(去掉 GLOBAL 等噪音)、时间 · 延时、站点名 —— 对齐 Open-Box 通知样式
      const time = fmtTime(new Date(sp.at * 1000).toISOString()).slice(5);
      lines.push(h('div', { class: 'tip-line chain' },
        chain.length > 1 ? chain.slice(1).join(' → ') : (chain[0] || '线路未知')));
      lines.push(h('div', { class: 'tip-line meta' },
        (sp.delay_ms > 0 ? sp.delay_ms + ' ms' : '失败') + ' · ' + time));
      lines.push(h('div', { class: 'tip-line svc' }, siteIcon(name), h('span', {}, name)));
      tipEl = h('div', { class: 'speed-tip' }, ...lines);
      document.body.append(tipEl);
      const r = bar.getBoundingClientRect();
      const tw = 260;
      tipEl.style.left = Math.max(8, Math.min(r.left, window.innerWidth - tw - 8)) + 'px';
      tipEl.style.top = (r.top - tipEl.offsetHeight - 8) + 'px';
      if (r.top - tipEl.offsetHeight - 8 < 8) {
        tipEl.style.top = (r.bottom + 8) + 'px';
      }
    }, 80);
  });
  bar.addEventListener('mouseleave', hideSpeedTip);
}

// 历史缓存:每次测速结果连同线路一起记入
const speedLocalHist = {};

function pushLocalHist(name, delay, at, chain) {
  (speedLocalHist[name] = speedLocalHist[name] || []).push({ at: at || Date.now() / 1000, delay_ms: delay, chain: chain || '' });
  if (speedLocalHist[name].length > 60) speedLocalHist[name].shift();
}

// ---------------------------------------------------------------------------
// 流量统计(对齐 Open-Box 底部卡片):出站节点 / 终端设备 / 访问目标 三个页签
// ---------------------------------------------------------------------------

let trafficTab = localStorage.getItem('boxhub.trafficTab') || 'outbound';
let trafficExpanded = new Set();

function trafficCard() {
  const tabs = ['outbound', 'clients', 'targets'];
  const tabNames = { outbound: '出站节点', clients: '终端设备', targets: '访问目标' };
  const body = h('div');
  const tabBar = h('div', { class: 'row', style: { gap: '6px', marginBottom: '10px' } },
    ...tabs.map((t) => h('button', {
      class: 'sm', 'data-tab': t,
      onclick: () => { trafficTab = t; localStorage.setItem('boxhub.trafficTab', t); draw(); },
    }, tabNames[t])));
  const resetBtn = h('button', {
    class: 'sm ghost', title: '清零重新统计',
    onclick: async () => {
      if (!confirm('清零所有流量统计并重新开始?')) return;
      try { await api('GET', '/api/traffic?action=reset'); draw(); toast('已清零', 'ok'); }
      catch (e) { toast(e.message, 'err'); }
    },
  }, '↺ 重置');
  function syncTab() {
    tabBar.querySelectorAll('button[data-tab]').forEach((b) => {
      b.classList.toggle('primary', b.dataset.tab === trafficTab);
      const cnt = (b.dataset.count = b.dataset.count || '');
      b.textContent = tabNames[b.dataset.tab] + cnt;
    });
  }
  function toggleRow(name) {
    if (trafficExpanded.has(name)) trafficExpanded.delete(name);
    else trafficExpanded.add(name);
    draw();
  }
  async function draw() {
    // 页签状态提到模块级 + localStorage:之前存在组件闭包里,
    // 仪表盘每 5 秒自动刷新会重建卡片,切到「终端设备」几秒后就被重置回「出站节点」。
    syncTab();
    try {
      const r = await api('GET', '/api/traffic');
      const rows = r[trafficTab] || [];
      tabBar.querySelectorAll('button[data-tab]').forEach((b) => {
        b.textContent = tabNames[b.dataset.tab] + ' (' + ((r[b.dataset.tab] || []).length) + ')';
      });
      syncTab();
      if (!rows.length) {
        body.replaceChildren(h('div', { class: 'small dim', style: { padding: '8px 2px' } },
          '暂无流量数据。局域网设备通过本机代理(7890)或 TUN 产生流量后,这里会按节点/设备/目标累计统计。'));
        return;
      }
      const trs = [];
      for (const x of rows) {
        const expanded = trafficExpanded.has(x.name);
        const arrow = h('button', {
          class: 'sm ghost row-toggle', style: { padding: '2px 6px', transform: expanded ? 'rotate(90deg)' : '' },
          onclick: () => toggleRow(x.name), title: '展开/收起',
        }, '▸');
        trs.push(h('tr', { style: { cursor: 'pointer' }, onclick: (e) => { if (!e.target.closest('button, a')) toggleRow(x.name); } },
          h('td', {}, h('div', { class: 'row', style: { gap: '6px' } }, arrow,
            h('span', { style: { overflowWrap: 'anywhere' } }, x.name))),
          h('td', { class: 'mono small nowrap' }, fmtBytes(x.download)),
          h('td', { class: 'mono small nowrap' }, fmtBytes(x.upload)),
          h('td', { class: 'mono small nowrap', style: { fontWeight: '600' } }, fmtBytes(x.total)),
          h('td', {}, h('div', { class: 'traffic-pct' },
            h('span', { class: 'small dim' }, Math.round(x.percent) + '%'),
            h('div', { class: 'bar' }, h('i', { class: 'green', style: { width: Math.max(2, Math.min(100, x.percent)) + '%' } }))))));
        if (expanded) {
          const by = x.by || [];
          trs.push(h('tr', { class: 'traffic-detail-row' },
            h('td', { colspan: 5 }, by.length === 0
              ? h('div', { class: 'small dim', style: { padding: '6px 10px' } }, '暂无明细(等待新的流量写入,或连接尚未被采样)')
              : h('table', { class: 'mini-table' },
                  h('thead', {}, h('tr', {},
                    h('th', {}, trafficTab === 'outbound' ? '访问目标' : '落地节点'),
                    h('th', {}, '进站'), h('th', {}, '出站'), h('th', {}, '总流量'), h('th', {}, '占比'))),
                  h('tbody', {}, ...by.map((b) => h('tr', {},
                    h('td', { style: { overflowWrap: 'anywhere' } }, b.name),
                    h('td', { class: 'mono small nowrap' }, fmtBytes(b.download)),
                    h('td', { class: 'mono small nowrap' }, fmtBytes(b.upload)),
                    h('td', { class: 'mono small nowrap' }, fmtBytes(b.total)),
                    h('td', {}, h('div', { class: 'traffic-pct' },
                      h('span', { class: 'small dim' }, Math.round(b.percent) + '%'),
                      h('div', { class: 'bar' }, h('i', { style: { width: Math.max(2, Math.min(100, b.percent)) + '%' } }))))) ))))));
        }
      }
      body.replaceChildren(h('table', {},
        h('thead', {}, h('tr', {},
          h('th', {}, '名称'), h('th', { class: 'nowrap' }, '进站'), h('th', { class: 'nowrap' }, '出站'),
          h('th', { class: 'nowrap' }, '总流量'), h('th', { style: { width: '30%' } }, '占比'))),
        h('tbody', {}, ...trs)));
    } catch (e) {
      body.replaceChildren(h('span', { class: 'badge err' }, e.message));
    }
  }
  const el = card('流量统计', '按内核连接快照采样累计;短连接可能少量漏计', '⇅', 'i-green', tabBar, body,
    h('div', { class: 'row', style: { marginTop: '10px' } }, h('span', { class: 'spacer' }), resetBtn));
  draw();
  return el;
}
