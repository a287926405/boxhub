/* BoxHub 控制台 — 功能页面:DNS 监控 / 节点 / 筛选 / 规则 / 链式代理 / 高级配置 / 内核面板 / 日志 / 设置
 *
 * 依赖 core.js 提供的全局符号:h / api / toast / kv / state / navTo / statCard 等。
 */
'use strict';

// ---------------------------------------------------------------------------
// 通用小部件
// ---------------------------------------------------------------------------

function head(title, actions) {
  return h('div', { class: 'row', style: { marginBottom: '18px' } },
    h('h1', { class: 'page-title', style: { margin: 0 } }, title),
    h('div', { class: 'spacer' }),
    actions || null);
}

function card(title, sub, ico, icoCls, ...content) {
  return h('div', { class: 'card' },
    title ? h('div', { class: 'card-head' },
      ico ? h('div', { class: 'ico ' + (icoCls || 'i-blue') }, ico) : null,
      h('div', {}, h('div', { class: 'card-title' }, title),
        sub ? h('div', { class: 'card-sub' }, sub) : null)) : null,
    ...content);
}

function emptyState(icon, text, hint, action) {
  return h('div', { class: 'empty' },
    h('div', { class: 'icon' }, icon), h('div', {}, text),
    hint ? h('div', { class: 'small', style: { marginTop: '8px' } }, hint) : null,
    action ? h('div', { style: { marginTop: '14px' } }, action) : null);
}

function field(label, input, hint) {
  return h('div', { class: 'field' },
    h('label', {}, label), input,
    hint ? h('div', { class: 'hint' }, hint) : null);
}

function checkField(label, input, hint) {
  return h('div', { class: 'field check' },
    h('label', {}, input, label), hint ? h('div', { class: 'hint' }, hint) : null);
}

// ---------------------------------------------------------------------------
// DNS 监控
// ---------------------------------------------------------------------------

async function renderDNS(root) {
  root.append(head('DNS 监控', h('button', {
    class: 'sm', onclick: () => renderPage(),
  }, '刷新')));

  const body = h('div');
  root.append(body);

  const draw = async () => {
    clear(body);
    try {
      const cfg = (await api('GET', '/api/config')).config;
      state.config = cfg; // DNS 分流卡要读最新值,不同步会拿别的页面的陈旧快照
      const dns = cfg.dns || {};
      const stats = await api('GET', '/api/dns/stats').catch(() => null);

      // 统计卡放最上面,与仪表盘"统计先行"的视觉节奏一致
      if (stats && stats.available) {
        body.append(h('div', { class: 'grid g4' },
          statCard('◎', 'i-blue', '总查询数', String(stats.total || 0)),
          statCard('✓', 'i-green', '成功查询', String(stats.success || 0)),
          statCard('✗', 'i-red', 'NXDOMAIN', String(stats.nxdomain || 0)),
          statCard('⚡', 'i-orange', '平均响应', (stats.avg_ms || 0).toFixed(2) + ' ms')));
      }

      body.append(h('div', { class: 'card' },
        h('div', { class: 'card-head' },
          h('div', { class: 'ico i-blue' }, '◎'),
          h('div', {}, h('div', { class: 'card-title' }, 'DNS 配置'),
            h('div', { class: 'card-sub' }, '当前生效的解析设置'))),
        kv('直连 DNS', (dns.direct_servers || []).join(', ') || '—'),
        kv('代理 DNS', (dns.proxy_servers || []).join(', ') || '—'),
        kv('兜底', dns.final_server || 'proxy'),
        kv('FakeIP', dns.fake_ip ? h('span', { class: 'badge ok' }, '已启用') : h('span', { class: 'badge' }, '未启用')),
        kv('策略', dns.strategy || 'prefer_ipv4'),
        kv('劫持', dns.hijack ? '开启' : '关闭'),
        h('div', { style: { marginTop: '12px' } },
          h('button', { class: 'sm', onclick: () => navTo('settings') }, '修改设置'))));

      // ---- DNS 分流模式(对齐 SMBox)----
      body.append(dnsModeCard(cfg));

      if (stats && stats.available) {
        body.append(card('上游服务器统计', '共 ' + (stats.upstreams || []).length + ' 个上游', '⇅', 'i-purple',
          h('table', {},
            h('thead', {}, h('tr', {},
              h('th', {}, '上游'), h('th', {}, '查询'), h('th', {}, '成功'),
              h('th', {}, 'NXDOMAIN'), h('th', {}, '失败'), h('th', {}, '平均'), h('th', {}, '成功率'))),
            h('tbody', {}, ...(stats.upstreams || []).map((u) => h('tr', {},
              h('td', {}, u.name),
              h('td', { class: 'mono' }, String(u.total)),
              h('td', { class: 'mono', style: { color: 'var(--green)' } }, String(u.success)),
              h('td', { class: 'mono' }, String(u.nxdomain)),
              h('td', { class: 'mono', style: { color: u.failed ? 'var(--red)' : 'inherit' } }, String(u.failed)),
              h('td', { class: 'mono' }, (u.avg_ms || 0).toFixed(1) + 'ms'),
              h('td', {}, h('span', { class: 'badge ' + (u.rate >= 95 ? 'ok' : u.rate >= 70 ? 'warn' : 'err') },
                (u.rate || 0).toFixed(1) + '%'))))))));
      } else {
        body.append(card('实时 DNS 统计', '', '◎', 'i-green',
          h('div', { class: 'small dim' },
            'BoxHub 使用 sing-box 内核处理 DNS,详细的查询统计请在「',
            h('a', { href: '#kernel-ui', onclick: (e) => { e.preventDefault(); navTo('kernel-ui'); } }, '内核面板'),
            '」→ 日志中查看(内核的 DNS 日志需在设置里把日志级别调为 debug)。')));
      }
    } catch (e) {
      clear(body);
      body.append(card('DNS 监控', '', '◎', 'i-red', h('span', { class: 'badge err' }, e.message)));
    }
  };

  await draw();
  startRefresh(draw, 8000);
}

// ---------------------------------------------------------------------------
// 节点与订阅
// ---------------------------------------------------------------------------

async function renderNodes(root) {
  root.append(head('节点', h('button', { class: 'primary sm', onclick: addSubscription }, '＋ 添加订阅')));
  const cfg = (await api('GET', '/api/config')).config;
  state.config = cfg;
  const nodes = cfg.nodes || [];
  const subs = cfg.subscriptions || [];

  // 订阅
  root.append(card('订阅', subs.length + ' 个订阅 · ' + nodes.length + ' 个节点', '▥', 'i-purple',
    subs.length === 0
      ? emptyState('▥', '还没有订阅', '支持 Clash YAML、base64 分享链接和多行节点链接')
      : h('table', {},
          h('thead', {}, h('tr', {},
            h('th', {}, '名称'), h('th', {}, '状态'), h('th', {}, '节点'),
            h('th', {}, '最近更新'), h('th', {}, ''))),
          h('tbody', {}, ...subs.map((s) => {
            const cnt = nodes.filter((n) => n.source === 'sub:' + s.id).length;
            return h('tr', {},
              h('td', {}, h('div', { style: { fontWeight: '500' } }, s.name),
                h('div', { class: 'small mute mono' }, (s.url || '').slice(0, 46) + '…')),
              h('td', {}, s.last_error
                ? h('span', { class: 'badge err', title: s.last_error }, '出错')
                : h('span', { class: 'badge ok' }, '正常')),
              h('td', { class: 'mono' }, String(cnt)),
              h('td', { class: 'small dim' }, relTime(s.last_update)),
              h('td', { class: 'nowrap' },
                h('button', { class: 'sm', onclick: () => editSubscription(s) }, '编辑'), ' ',
                h('button', { class: 'sm', onclick: () => refreshSub(s) }, '刷新'), ' ',
                h('button', { class: 'sm danger', onclick: () => deleteSub(s) }, '删除')));
          })))));

  // 节点列表(按来源分组,量大时只渲染前 500 条)
  const shown = nodes.slice(0, 500);
  const rows = shown.map((n) => h('tr', {},
    h('td', {}, n.name, n.disabled ? h('span', { class: 'badge warn', style: { marginLeft: '6px' } }, '禁用') : null),
    h('td', {}, h('span', { class: 'badge' }, n.type)),
    h('td', { class: 'mono small' }, n.server + ':' + n.port),
    h('td', { class: 'small dim' }, n.source === 'manual' ? '手动' : '订阅'),
    h('td', {}, h('button', { class: 'sm danger', onclick: () => deleteNode(n) }, '删除'))));

  const nodeTable = h('div', { style: { maxHeight: '600px', overflow: 'auto' } },
    h('table', {},
      h('thead', {}, h('tr', {},
        h('th', {}, '名称'), h('th', {}, '协议'), h('th', {}, '服务器'),
        h('th', {}, '来源'), h('th', {}, ''))),
      h('tbody', {}, ...rows)));

  const nodeCard = card('节点', nodes.length + ' 个', '▤', 'i-blue',
    nodes.length === 0 ? emptyState('▤', '还没有节点', '先添加订阅,或手动粘贴分享链接') : nodeTable,
    nodes.length > 500 ? h('div', { class: 'small dim', style: { marginTop: '10px' } },
      '共 ' + nodes.length + ' 个节点,仅显示前 500 个') : null,
    h('div', { style: { marginTop: '12px' } },
      h('button', { class: 'sm', onclick: addManualNode }, '＋ 手动添加')));
  root.append(nodeCard);
}

function addSubscription() {
  const name = h('input', { placeholder: '例如:我的机场' });
  const url = h('input', { placeholder: 'https://example.com/api/v1/client/subscribe?token=...' });
  const ua = h('input', { placeholder: '留空使用 Clash/v1.18.0' });
  const msg = h('div', { class: 'small', style: { color: 'var(--red)', minHeight: '20px' } });
  openModal('添加订阅', h('div', {},
    field('名称 *', name),
    field('订阅地址 *', url),
    field('User-Agent', ua, '部分机场按 UA 返回不同格式;返回 Clash YAML 时用默认值即可'),
    msg), async () => {
    if (!name.value.trim()) throw new Error('请填写名称');
    if (!/^https?:\/\//.test(url.value)) throw new Error('地址必须以 http:// 或 https:// 开头');
    msg.textContent = '正在拉取订阅…';
    const r = await api('POST', '/api/subscriptions', { name: name.value.trim(), url: url.value.trim(), user_agent: ua.value.trim() });
    closeModal();
    if (r.warning) toast(r.warning, 'warn', 6000); else toast('订阅已添加', 'ok');
    renderPage();
  });
}

// editSubscription 编辑订阅:名称/地址/UA/关键词过滤/自动更新间隔/启停。
// 改地址会自动用新地址重拉一次,并沿用节点稳定 ID 对账,节点组不会错位。
function editSubscription(sub) {
  const name = h('input', { value: sub.name || '' });
  const url = h('input', { value: sub.url || '' });
  const ua = h('input', { value: sub.user_agent || '', placeholder: '留空使用 Clash/v1.18.0' });
  const filter = h('input', { value: sub.filter || '', placeholder: '例如:香港(只导入名称含该词的节点)' });
  const interval = h('input', { type: 'number', value: sub.update_interval || 0, min: 0, placeholder: '0' });
  const enabled = h('input', { type: 'checkbox', checked: sub.enabled !== false });
  const msg = h('div', { class: 'small', style: { color: 'var(--red)', minHeight: '20px' } });
  openModal('编辑订阅', h('div', {},
    field('名称 *', name),
    field('订阅地址 *', url, '修改地址后会立即用新地址重新拉取'),
    field('User-Agent', ua, '返回 Clash YAML 时用默认值即可'),
    field('关键词过滤', filter, '留空导入全部节点'),
    field('自动更新间隔(分钟)', interval, '0 = 跟随面板全局设置'),
    checkField('启用', enabled),
    msg), async () => {
    if (!name.value.trim()) throw new Error('请填写名称');
    if (!/^https?:\/\//.test(url.value.trim())) throw new Error('地址必须以 http:// 或 https:// 开头');
    msg.textContent = '保存中…';
    const r = await api('PUT', '/api/subscriptions/' + sub.id, {
      name: name.value.trim(), url: url.value.trim(), user_agent: ua.value.trim(),
      filter: filter.value.trim(), update_interval: parseInt(interval.value, 10) || 0,
      enabled: enabled.checked,
    });
    closeModal();
    if (r.warning) toast(r.warning, 'warn', 7000);
    else toast('订阅已更新', 'ok');
    renderPage();
  });
}

async function refreshSub(s) {
  toast('正在刷新订阅…', 'info', 1500);
  try {
    await api('POST', '/api/subscriptions/' + s.id + '/refresh');
    toast('订阅已刷新', 'ok');
    renderPage();
  } catch (e) { toast(e.message, 'err', 6000); }
}

async function deleteSub(s) {
  if (!confirm('删除订阅「' + s.name + '」?它带来的节点也会一并删除。')) return;
  try { await api('DELETE', '/api/subscriptions/' + s.id); toast('已删除', 'ok'); renderPage(); }
  catch (e) { toast(e.message, 'err', 6000); }
}

function addManualNode() {
  const name = h('input', { placeholder: '节点名称(可留空)' });
  const text = h('textarea', { placeholder: '粘贴分享链接,例如:\nss://...\nvmess://...\nvless://...\ntrojan://...\nhysteria2://...', style: { height: '140px' } });
  const msg = h('div', { class: 'small', style: { color: 'var(--red)', minHeight: '20px' } });
  openModal('手动添加节点', h('div', {},
    field('名称', name),
    field('分享链接 *', text, '支持 ss / vmess / vless / trojan / hysteria2 / tuic,可一次粘多行'),
    msg), async () => {
    if (!text.value.trim()) throw new Error('请粘贴分享链接');
    msg.textContent = '正在解析…';
    const r = await api('POST', '/api/nodes/parse', { text: text.value, name: name.value.trim() });
    closeModal();
    if (r.skipped && r.skipped.length) toast('部分节点无法解析:' + r.skipped.join('; '), 'warn', 6000);
    toast('已添加 ' + r.added + ' 个节点', 'ok');
    renderPage();
  });
}

async function deleteNode(n) {
  if (!confirm('删除节点「' + n.name + '」?')) return;
  try { await api('DELETE', '/api/nodes/' + n.id); toast('已删除', 'ok'); renderPage(); }
  catch (e) { toast(e.message, 'err', 6000); }
}

// ---------------------------------------------------------------------------
// 筛选
// ---------------------------------------------------------------------------

async function renderFilters(root) {
  root.append(head('筛选', h('div', { class: 'row' },
    h('button', { class: 'sm', onclick: autoCreateFilters }, '按地区自动生成'),
    h('button', { class: 'primary sm', onclick: () => editFilter(null) }, '＋ 新建筛选'))));

  const d = await api('GET', '/api/filters');
  state.filters = d.filters || [];

  let overviewBody;
  if (state.filters.length === 0) {
    overviewBody = emptyState('⛃', '还没有筛选',
      '筛选会按节点名称里的关键词/地区自动归类,订阅更新后自动跟随,不用手工维护',
      h('button', { class: 'primary sm', onclick: autoCreateFilters }, '按地区自动生成'));
  } else {
    const items = state.filters.map(function (f) {
      return h('div', {
        class: 'filter-item', style: { cursor: 'pointer' },
        onclick: function () { showFilterNodes(f); },
      },
        h('span', { class: 'name' }, fDisplay(f),
          !f.enabled ? h('span', { class: 'badge', style: { marginLeft: '6px' } }, '停用') : null),
        h('span', { class: 'cnt' }, f.count + ' 个'));
    });
    overviewBody = h('div', { class: 'filter-grid' }, ...items);
  }
  root.append(card('筛选概览',
    '共 ' + state.filters.length + ' 个筛选,覆盖 ' + (d.total || 0) + ' 个节点',
    '⛃', 'i-blue', overviewBody));

  if (state.filters.length) {
    root.append(card('筛选列表', '', '☰', 'i-purple',
      h('table', {},
        h('thead', {}, h('tr', {},
          h('th', {}, '名称'), h('th', {}, '关键词'), h('th', {}, '节点'),
          h('th', {}, '被规则引用'), h('th', {}, ''))),
        h('tbody', {}, ...state.filters.map((f) => h('tr', {},
          h('td', {}, h('div', { style: { fontWeight: '500' } }, fDisplay(f)),
            h('div', { class: 'small mute' }, f.region_name || '')),
          h('td', { class: 'small dim', style: { maxWidth: '240px' } },
            (f.keywords || []).join(', ') || (f.regex ? '正则: ' + f.regex : '—')),
          h('td', {}, h('span', { class: 'badge info' }, f.count + ' 个')),
          h('td', { class: 'small dim' }, (f.references || []).join('、') || '—'),
          h('td', {},
            h('button', { class: 'sm', onclick: () => showFilterNodes(f) }, '查看'), ' ',
            h('button', { class: 'sm', onclick: () => editFilter(f) }, '编辑'), ' ',
            h('button', { class: 'sm danger', onclick: () => deleteFilter(f) }, '删除'))))))));
  }
}

async function showFilterNodes(f) {
  try {
    const d = await api('GET', '/api/filters/' + f.id);
    const nodes = d.nodes || [];
    openModal('筛选「' + f.name + '」命中 ' + nodes.length + ' 个节点', h('div', {},
      nodes.length === 0
        ? emptyState('▤', '没有节点命中', '检查关键词是否与订阅节点名称匹配')
        : h('div', { style: { maxHeight: '420px', overflow: 'auto' } },
            h('table', {},
              h('thead', {}, h('tr', {}, h('th', {}, '名称'), h('th', {}, '协议'), h('th', {}, '服务器'))),
              h('tbody', {}, ...nodes.map((n) => h('tr', {},
                h('td', {}, n.name),
                h('td', {}, h('span', { class: 'badge' }, n.type)),
                h('td', { class: 'mono small' }, n.server + ':' + n.port))))))));
  } catch (e) { toast(e.message, 'err', 6000); }
}

function editFilter(f) {
  const isNew = !f;
  const d = f || {
    name: '', keywords: [], keywords_and: [], exclude: [], regex: '',
    region: '', sort: 'name', enabled: true, max_nodes: 0, count_only: false,
  };
  const name = h('input', { value: d.name, placeholder: '例如:日本 / CF 节点' });
  let iconVal = d.icon || '';
  const iconPrev = h('span', { class: 'icon-preview' });
  const iconBox = h('div', {});
  const iconBtn = h('button', { class: 'sm' }, '选择图标');
  iconBtn.addEventListener('click', () => {
    if (iconBox.querySelector('.icon-picker')) { iconBox.replaceChildren(); return; }
    iconBox.replaceChildren(iconPickerPanel((v) => { iconVal = v; paintIcon(); iconBox.replaceChildren(); }));
  });
  function paintIcon() { iconPrev.textContent = iconVal || '无'; }
  paintIcon();
  const kws = h('input', { value: (d.keywords || []).join(','), placeholder: '日本,JP,东京(逗号分隔,任一命中)' });
  const kwsAnd = h('input', { value: (d.keywords_and || []).join(','), placeholder: '必须全部包含的关键词' });
  const exclude = h('input', { value: (d.exclude || []).join(','), placeholder: '排除词,如:备用,过期' });
  const regex = h('input', { value: d.regex || '', placeholder: '可选,如:^JP\\s+\\d+$' });
  const region = h('select', {},
    h('option', { value: '', selected: !d.region }, '不指定'),
    ...['jp', 'us', 'hk', 'tw', 'sg', 'kr', 'gb', 'de', 'fr', 'nl', 'ru', 'ca', 'au', 'cn', 'other'].map((c) =>
      h('option', { value: c, selected: d.region === c }, regionLabel(c))));
  const mode = h('select', {},
    ...[['select', '手动选择'], ['urltest', '自动择优']].map(([v, n]) =>
      h('option', { value: v, selected: (d.mode || 'select') === v }, n)));
  const sort = h('select', {},
    ...[['name', '按名称'], ['region', '按地区'], ['none', '保持原顺序']].map(([v, n]) =>
      h('option', { value: v, selected: d.sort === v }, n)));
  const maxN = h('input', { type: 'number', value: d.max_nodes || 0, min: 0 });
  const enabled = h('input', { type: 'checkbox', checked: d.enabled !== false });
  const countOnly = h('input', { type: 'checkbox', checked: !!d.count_only });
  const msg = h('div', { class: 'small', style: { color: 'var(--red)', minHeight: '20px' } });
  const split = (v) => v.split(/[,，]/).map((x) => x.trim()).filter(Boolean);

  openModal(isNew ? '新建筛选' : '编辑筛选', h('div', {},
    h('div', { class: 'grid g2' },
      field('名称 *', name),
      field('图标', h('div', {}, h('div', { class: 'row', style: { gap: '8px', alignItems: 'center' } }, iconPrev, iconBtn), iconBox),
        '会拼进出站名,内核面板里同样显示')),
    
    field('关键词(任一命中)', kws, '节点名称里包含任意一个关键词就入选'),
    field('必须同时包含', kwsAnd, '多个关键词用逗号分隔,需全部包含'),
    field('排除关键词', exclude, '命中则排除,用于剔除「备用」「过期」这类节点'),
    field('正则(可选)', regex),
    h('div', { class: 'grid g2' },
      field('组类型', mode, '手动选择 = 自己挑线路;自动择优 = 自动选最快'),
      field('地区标记', region, '仅用于界面显示国旗')),
    h('div', { class: 'grid g2' },
      field('排序', sort),
      field('最多收录节点数', maxN, '0 表示不限')),
    checkField('启用', enabled),
    checkField('仅统计(不生成出站)', countOnly, '勾选后只统计数量,不用于分流'),
    msg), async () => {
    msg.textContent = '';
    if (!name.value.trim()) throw new Error('请填写名称');
    const body = {
      name: name.value.trim(),
      icon: iconVal,
      keywords: split(kws.value),
      keywords_and: split(kwsAnd.value),
      exclude: split(exclude.value),
      regex: regex.value.trim(),
      region: region.value,
      mode: mode.value,
      sort: sort.value,
      max_nodes: parseInt(maxN.value, 10) || 0,
      enabled: enabled.checked,
      count_only: countOnly.checked,
    };
    if (!body.keywords.length && !body.keywords_and.length && !body.regex) {
      throw new Error('至少要填一个关键词或正则');
    }
    if (isNew) await api('POST', '/api/filters', body);
    else await api('PUT', '/api/filters/' + f.id, body);
    toast('已保存', 'ok');
    closeModal();
    renderPage();
  });
}


// ---------------------------------------------------------------------------
// 图标选择器(对齐 Open-Box 的分组图标,emoji 版)
// 库内置在面板里,离线可用;选择存进筛选的 icon 字段,
// 拼进显示名与内核出站 tag —— zashboard 里也能看到。
// ---------------------------------------------------------------------------

const ICON_CATS = [
  { key: 'region', name: '地区', items: [
    ['🇯🇵','日本 japan jp 东京 tokyo'],['🇺🇸','美国 usa us 美'],['🇭🇰','香港 hk hongkong'],
    ['🇸🇬','新加坡 sg singapore'],['🇰🇷','韩国 kr korea 首尔'],['🇹🇼','台湾 tw taiwan'],
    ['🇬🇧','英国 gb uk 伦敦 london'],['🇩🇪','德国 de germany'],['🇫🇷','法国 fr france'],
    ['🇳🇱','荷兰 nl netherlands'],['🇷🇺','俄罗斯 ru russia'],['🇨🇦','加拿大 ca canada'],
    ['🇦🇺','澳大利亚 au australia'],['🇮🇳','印度 in india'],['🇹🇭','泰国 th thailand'],
    ['🇻🇳','越南 vn'],['🇲🇾','马来西亚 my'],['🇮🇩','印尼 id'],
    ['🇦🇪','阿联酋 ae dubai'],['🇹🇷','土耳其 tr'],['🇨🇭','瑞士 ch'],['🇸🇪','瑞典 se'],
    ['🇧🇷','巴西 br'],['🇿🇦','南非 za'],['🇳🇿','新西兰 nz'],['🇫🇮','芬兰 fi'],
    ['🇮🇪','爱尔兰 ie'],['🇵🇱','波兰 pl'],['🇪🇸','西班牙 es'],['🇵🇹','葡萄牙 pt'],
    ['🇮🇹','意大利 it'],['🇻🇬','英属维尔京 bvi'],['🇸🇦','沙特 sa'],['🇮🇱','以色列 il'],
    ['🇰🇿','哈萨克斯坦 kz'],['🇵🇭','菲律宾 ph'],['🇧🇩','孟加拉 bd'] ]},
  { key: 'hot', name: '常用', items: [
    ['🔥','热门 hot fire'],['⚡','快速 speed 闪'],['🚀','加速 rocket'],['🌐','国际 global net'],
    ['💎','钻石 diamond 高级'],['🎯','目标 target'],['⭐','星标 star'],['🏆','冠军 cup'],
    ['🎁','赠品 gift'],['💰','价格 money'],['🔒','安全 lock 私享'],['🛡️','防护 shield'],
    ['🧲','吸引 magnet'],['🌈','彩虹 rainbow'],['🍀','幸运 clover'],['⚠️','注意 warn'] ]},
  { key: 'tech', name: '科技', items: [
    ['🤖','机器人 ai bot'],['🧠','大脑 brain ai'],['🛰️','卫星 sat'],['📡','天线 ant'],
    ['💡','灵感 idea'],['⚙️','齿轮 gear 设置'],['🔌','插头 plug'],['💾','存档 save disk'],
    ['🖥️','电脑 vps pc server'],['📶','信号 wifi'],['🧿','护符 eye'],['🔮','水晶球 magic'],
    ['🪐','星球 planet orbit'],['💻','笔记本 laptop'],['🖨️','打印 print'] ]},
  { key: 'media', name: '影音', items: [
    ['🎵','音乐 music'],['🎬','影视 video movie'],['📺','电视 tv'],['🎮','游戏 game'],
    ['📷','相机 photo'],['🎙️','播客 mic'],['📻','收音机 radio'],['🎧','耳机 ear'],
    ['🍿','追剧 snack'],['🏀','体育 sport ball'],['⚽','足球 foot'] ]},
  { key: 'animal', name: '动物', items: [
    ['🐙','章鱼 octopus deepseek'],['🦊','狐狸 fox'],['🐼','熊猫 panda cn'],
    ['🐧','企鹅 penguin linux'],['🦉','猫头鹰 owl'],['🐬','海豚 dolphin'],
    ['🦈','鲨鱼 shark'],['🐝','蜜蜂 bee'],['🦋','蝴蝶 butterfly'],['🐢','龟 turtle 稳定'],
    ['🦁','狮子 lion'],['🐯','老虎 tiger'],['🐨','考拉 koala au'],['🐺','狼 wolf'],
    ['🦅','鹰 eagle us'],['🐘','大象 elephant'] ]},
  { key: 'misc', name: '符号', items: [
    ['🔴','红点 red'],['🟠','橙点 orange'],['🟡','黄点 yellow'],['🟢','绿点 green'],
    ['🔵','蓝点 blue'],['🟣','紫点 purple'],['⚫','黑点 black'],['⚪','白点 white'],
    ['🔺','上三角 up'],['🔻','下三角 down'],['▶️','播放 play'],['♠️','黑桃 spade'],
    ['♦️','方块 diamond2'],['🔱','三叉 trident'],['1️⃣','一 1'],['2️⃣','二 2'],
    ['3️⃣','三 3'],['✖️','叉 x'] ]},
];

// 图标选择面板(内嵌在编辑弹窗里):openModal 是单例,独立弹窗会把
// 编辑弹窗顶掉、选中结果无处回填——所以做成可展开的内联面板。
function iconPickerPanel(onDone) {
  let cat = 'region', kw = '';
  const grid = h('div', { class: 'icon-grid' });
  const tabsBox = h('div', { class: 'row icon-tabs', style: { gap: '6px', margin: '8px 0' } });
  const search = h('input', {
    placeholder: '搜索图标(中/英/拼音)…', class: 'icon-search',
    oninput: () => { kw = search.value.trim().toLowerCase(); draw(); },
  });
  function draw() {
    let list = [];
    for (const c of ICON_CATS) {
      if (cat !== 'all' && c.key !== cat) continue;
      for (const it of c.items) list.push(it);
    }
    if (kw) list = list.filter((it) => (it[1] + ' ' + it[2]).toLowerCase().includes(kw));
    grid.replaceChildren(...(list.length ? list.map((it) => h('button', {
      class: 'icon-cell', title: it[1],
      onclick: () => onDone(it[0]),
    }, it[0])) : [h('div', { class: 'small dim' }, '没有匹配的图标')]));
    tabsBox.querySelectorAll('button[data-cat]').forEach((b) => {
      b.classList.toggle('primary', b.dataset.cat === cat);
    });
  }
  tabsBox.replaceChildren(
    ...[{ key: 'all', name: '全部' }, ...ICON_CATS].map((c) => h('button', {
      class: 'sm', 'data-cat': c.key,
      onclick: () => { cat = c.key; draw(); },
    }, c.name)));
  const clear = h('button', { class: 'sm ghost', onclick: () => onDone('') }, '✕ 无图标');
  draw();
  return h('div', { class: 'icon-picker' }, search, tabsBox, grid, clear);
}

// filter display 供列表页渲染(带图标优先,回退地区自动国旗)
function fDisplay(f) {
  if (f.icon) return f.icon + ' ' + f.name;
  return flagPrefix(f) + f.name;
}

// flagPrefix 只在名称里还没有 emoji 时才补国旗,避免重复显示。
function flagPrefix(f) {
  if (!f.flag) return '';
  if (/[\u{1F1E6}-\u{1F1FF}]/u.test(f.name || '')) return '';
  return f.flag + ' ';
}

function regionLabel(code) {
  const m = {
    jp: '🇯🇵 日本', us: '🇺🇸 美国', hk: '🇭🇰 香港', tw: '🇹🇼 台湾', sg: '🇸🇬 新加坡',
    kr: '🇰🇷 韩国', gb: '🇬🇧 英国', de: '🇩🇪 德国', fr: '🇫🇷 法国', nl: '🇳🇱 荷兰',
    ru: '🇷🇺 俄罗斯', ca: '🇨🇦 加拿大', au: '🇦🇺 澳大利亚', cn: '🇨🇳 中国', other: '其他',
  };
  return m[code] || code;
}

async function deleteFilter(f) {
  const refs = f.references || [];
  const msg = refs.length
    ? '「' + f.name + '」正被以下规则引用:\n\n' + refs.join('\n') + '\n\n删除后这些规则会被禁用。确定删除?'
    : '确定删除筛选「' + f.name + '」?';
  if (!confirm(msg)) return;
  try { await api('DELETE', '/api/filters/' + f.id); toast('已删除', 'ok'); renderPage(); }
  catch (e) { toast(e.message, 'err', 6000); }
}

// dnsModeCard 「DNS 分流模式」卡片:默认代理/默认直连 二选一 + 例外设备列表。
function dnsModeCard(cfg) {
  const dns = (cfg && cfg.dns) || {};
  const mode = dns.mode || 'proxy';
  const exceptions = dns.mode_exceptions || [];
  const radioProxy = h('input', { type: 'radio', name: 'dns-mode', value: 'proxy', checked: mode === 'proxy' });
  const radioDirect = h('input', { type: 'radio', name: 'dns-mode', value: 'direct', checked: mode === 'direct' });
  const chip = (ip) => h('span', {
    class: 'badge', style: { display: 'inline-flex', gap: '6px', alignItems: 'center' },
  }, ip, h('a', {
    href: 'javascript:void(0)', title: '移除', style: { color: 'var(--text-mute)' },
    onclick: async (e) => {
      e.stopPropagation();
      const next = exceptions.filter((x) => x !== ip);
      await saveDnsMode(currentMode(), next);
    },
  }, '×'));
  const currentMode = () => (radioProxy.checked ? 'proxy' : 'direct');
  const modeText = () => (currentMode() === 'direct'
    ? '全部设备用直连 DNS（拿到真实 IP，境外域名可能被污染）；例外设备走内核解析'
    : '全部设备走内核解析（境外域名用假地址，避免污染）；例外设备用直连 DNS');

  const desc = h('div', { class: 'small dim', style: { marginBottom: '12px' } }, modeText());
  const switchMode = async () => {
    desc.textContent = modeText();
    await saveDnsMode(currentMode(), exceptions);
  };
  radioProxy.addEventListener('change', switchMode);
  radioDirect.addEventListener('change', switchMode);

  const card = h('div', { class: 'card' },
    h('div', { class: 'card-head' },
      h('div', { class: 'ico i-orange' }, '⇄'),
      h('div', {}, h('div', { class: 'card-title' }, 'DNS 分流模式'),
        h('div', { class: 'card-sub' }, '只影响域名怎么解析，不影响流量走不走代理')),
      h('div', { class: 'spacer' }),
      h('button', {
        class: 'sm', onclick: () => editDnsExceptions(exceptions),
      }, '编辑')),
    desc,
    h('div', { class: 'row', style: { marginBottom: '12px', gap: '18px' } },
      h('label', { style: { display: 'flex', gap: '7px', alignItems: 'center', cursor: 'pointer' } }, radioProxy, '默认代理'),
      h('label', { style: { display: 'flex', gap: '7px', alignItems: 'center', cursor: 'pointer' } }, radioDirect, '默认直连')),
    h('div', { class: 'row' },
      h('span', { class: 'small dim' }, '例外设备',
        exceptions.length ? h('span', { class: 'badge info', style: { marginLeft: '6px' } }, String(exceptions.length)) : null),
      ...exceptions.map(chip),
      exceptions.length === 0 ? h('span', { class: 'small mute' }, '（无）') : null));
  return card;
}

async function saveDnsMode(mode, exceptions) {
  try {
    // 保存前重新拉一次配置:切换模式会重启内核(耗时几秒),期间用户
    // 可能已经做了下一次保存;用陈旧快照回填会把刚保存的字段(如 mode)
    // 静默覆盖回去。布尔/列表字段也必须原样带上,后端对它们是覆盖语义。
    const dns = (await (await api('GET', '/api/config')).config).dns || {};
    await api('PUT', '/api/settings', { dns: {
      ...dns,
      mode,
      mode_exceptions: exceptions,
    } });
    toast('DNS 分流模式已保存', 'ok');
    state.config = (await api('GET', '/api/config')).config;
    renderPage();
  } catch (e) {
    toast(e.message, 'err', 8000);
    renderPage(); // 回滚界面显示
  }
}

function editDnsExceptions(exceptions) {
  const ta = h('textarea', { placeholder: '每行一个设备 IP,例如:\n10.10.10.35\n10.10.10.42\n192.168.1.0/24', style: { height: '140px' } }, exceptions.join('\n'));
  const msg = h('div', { class: 'small', style: { color: 'var(--red)', minHeight: '20px' } });
  openModal('例外设备', h('div', {},
    h('div', { class: 'small dim', style: { marginBottom: '10px' } },
      '这些设备使用与「默认」相反的 DNS 路径(支持 IP 或 CIDR)。' +
      '透明代理(TUN)场景下,内核需要识别来源才能区分设备;端口代理场景按连接来源 IP 生效。'),
    ta, msg), async () => {
    const list = ta.value.split('\n').map((x) => x.trim()).filter(Boolean);
    await saveDnsMode(((state.config || {}).dns || {}).mode || 'proxy', list);
    closeModal();
  });
}

// ===========================================================================
// 设备分流模式(流量层面):按来源设备决定流量走不走代理。
//
// 与上面的「DNS 分流模式」是两件事:那张卡只决定域名怎么解析,
// 这张卡才决定流量到底进不进代理。两者互不影响,可以自由组合。
// ===========================================================================
function deviceModeCard(cfg) {
  const routing = (cfg && cfg.routing) || {};
  const mode = routing.mode || 'proxy';
  const exceptions = routing.mode_exceptions || [];
  const radioProxy = h('input', { type: 'radio', name: 'device-mode', value: 'proxy', checked: mode === 'proxy' });
  const radioDirect = h('input', { type: 'radio', name: 'device-mode', value: 'direct', checked: mode === 'direct' });
  const currentMode = () => (radioProxy.checked ? 'proxy' : 'direct');

  const modeText = () => (currentMode() === 'direct'
    ? '全部设备走直连，例外设备走代理'
    : '全部设备走代理，例外设备走直连');
  const desc = h('div', { class: 'small dim', style: { marginBottom: '12px' } }, modeText());

  const chip = (ip) => h('span', {
    class: 'badge', style: { display: 'inline-flex', gap: '6px', alignItems: 'center' },
  }, ip, h('a', {
    href: 'javascript:void(0)', title: '移除', style: { color: 'var(--text-mute)' },
    onclick: async (e) => {
      e.stopPropagation();
      await saveDeviceMode(currentMode(), exceptions.filter((x) => x !== ip));
    },
  }, '×'));

  const switchMode = async () => {
    desc.textContent = modeText();
    await saveDeviceMode(currentMode(), exceptions);
  };
  radioProxy.addEventListener('change', switchMode);
  radioDirect.addEventListener('change', switchMode);

  return h('div', { class: 'card' },
    h('div', { class: 'card-head' },
      h('div', { class: 'ico i-orange' }, '⇄'),
      h('div', {}, h('div', { class: 'card-title' }, '设备分流模式'),
        h('div', { class: 'card-sub' }, '按来源设备决定流量走不走代理')),
      h('div', { class: 'spacer' }),
      h('button', {
        class: 'sm', onclick: () => editDeviceExceptions(exceptions),
      }, '编辑')),
    desc,
    h('div', { class: 'row', style: { marginBottom: '12px', gap: '18px' } },
      h('label', { style: { display: 'flex', gap: '7px', alignItems: 'center', cursor: 'pointer' } }, radioProxy, '默认代理'),
      h('label', { style: { display: 'flex', gap: '7px', alignItems: 'center', cursor: 'pointer' } }, radioDirect, '默认直连')),
    h('div', { class: 'row' },
      h('span', { class: 'small dim' }, '例外设备',
        exceptions.length ? h('span', { class: 'badge info', style: { marginLeft: '6px' } }, String(exceptions.length)) : null),
      ...exceptions.map(chip),
      exceptions.length === 0 ? h('span', { class: 'small mute' }, '（无）') : null));
}

async function saveDeviceMode(mode, exceptions) {
  try {
    // 后端对 mode_exceptions 是覆盖语义,两个字段必须一起传,
    // 否则"只改模式"会把例外设备清空。
    await api('PUT', '/api/settings', { routing: { mode, mode_exceptions: exceptions } });
    toast('设备分流模式已保存', 'ok');
    state.config = (await api('GET', '/api/config')).config;
    renderPage();
  } catch (e) {
    toast(e.message, 'err', 8000);
    renderPage(); // 回滚界面显示
  }
}

function editDeviceExceptions(exceptions) {
  const ta = h('textarea', {
    placeholder: '每行一个设备 IP,例如:\n10.10.10.35\n10.10.10.42\n192.168.1.0/24',
    style: { height: '140px' },
  }, exceptions.join('\n'));
  openModal('例外设备', h('div', {},
    h('div', { class: 'small dim', style: { marginBottom: '10px' } },
      '这些设备的流量走与「默认」相反的路径(支持 IP 或 CIDR)。' +
      '按连接来源 IP 生效,透明代理与端口代理都适用。'),
    ta), async () => {
    const list = ta.value.split('\n').map((x) => x.trim()).filter(Boolean);
    const cur = ((state.config || {}).routing || {}).mode || 'proxy';
    await saveDeviceMode(cur, list);
    closeModal();
  });
}
