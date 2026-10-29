/* BoxHub 控制台 — 功能页面(续):规则 / 链式代理 / 高级配置 / 内核面板 / 日志 / 设置 / 弹窗 */
'use strict';

// ---------------------------------------------------------------------------
// 规则(目标分流)
// ---------------------------------------------------------------------------

// 出口选项:直连 / 屏蔽 / 节点组 / 筛选 / 链式代理
function targetOptions(selected) {
  const opts = [
    { v: 'self', n: '节点组(生成选线组,跟随 Proxy)' },
    { v: 'direct', n: '直连' },
    { v: 'block', n: '屏蔽' },
  ];
  for (const f of state.filters || []) opts.push({ v: 'filter:' + f.id, n: '⛃ 筛选:' + (f.icon ? f.icon + ' ' : '') + f.name });
  for (const g of (state.config && state.config.groups) || []) opts.push({ v: 'group:' + g.id, n: '☰ 节点组:' + g.name });
  for (const c of state.chains || []) opts.push({ v: 'chain:' + c.id, n: '⛓ 链式:' + c.name });
  return opts.map((o) => h('option', { value: o.v, selected: o.v === selected }, o.n));
}

function targetLabel(t, asGroup, name) {
  if (asGroup) return h('span', {}, h('span', { class: 'dim' }, '跟随 '), name);
  if (t === 'direct') return '直连';
  if (t === 'block') return '屏蔽';
  if (t === 'self') return '走代理';
  if (t.startsWith('filter:')) {
    const f = (state.filters || []).find((x) => x.id === t.slice(7));
    return f ? '⛃ ' + f.name : t;
  }
  if (t.startsWith('group:')) {
    const g = ((state.config && state.config.groups) || []).find((x) => x.id === t.slice(6));
    return g ? g.name : t;
  }
  if (t.startsWith('chain:')) {
    const c = (state.chains || []).find((x) => x.id === t.slice(6));
    return c ? '⛓ ' + c.name : t;
  }
  if (t.startsWith('node:')) {
    const n = ((state.config && state.config.nodes) || []).find((x) => x.id === t.slice(5));
    return n ? n.name : t;
  }
  return t;
}

// 规则集 URL 很长,直接展示会把表格撑乱。
// 这里只显示规则集的"短名"(取最后一段并去掉扩展名),完整地址放在 title 里。
function shortRuleSet(u) {
  const s = String(u);
  const i = s.lastIndexOf('/');
  const base = i >= 0 ? s.slice(i + 1) : s;
  return base.replace(/\.(srs|json)$/i, '');
}

function matchText(m) {
  const parts = [];
  if (m && m.rule_sets && m.rule_sets.length) {
    const names = m.rule_sets.map(shortRuleSet);
    parts.push('规则集:' + (names.length > 3
      ? names.slice(0, 3).join(',') + ' 等 ' + names.length + ' 个'
      : names.join(',')));
  }
  const defs = [
    ['domain_suffix', '后缀'], ['domain', '域名'], ['domain_keyword', '关键词'],
    ['ip_cidr', 'IP段'], ['geo_site', '站点集'], ['geo_ip', 'IP库'],
    ['port', '端口'], ['network', '协议'],
  ];
  for (const [k, label] of defs) {
    if (m && m[k] && m[k].length) parts.push(label + ':' + m[k].join(','));
  }
  return parts.join(' · ') || '(空)';
}

let rulesViewMode = localStorage.getItem('boxhub.rulesView') || 'card';

function setRulesView(mode) {
  rulesViewMode = mode;
  localStorage.setItem('boxhub.rulesView', mode);
  renderPage();
}

async function renderRules(root) {
  root.append(head('规则', h('div', { class: 'row' },
    h('button', {
      class: 'sm' + (rulesViewMode === 'card' ? ' primary' : ''),
      title: '卡片模式', onclick: () => setRulesView('card'),
    }, '▦ 卡片'),
    h('button', {
      class: 'sm' + (rulesViewMode === 'table' ? ' primary' : ''),
      title: '表格模式', onclick: () => setRulesView('table'),
    }, '☰ 表格'),
    h('button', { class: 'sm', onclick: showRuleTemplates }, '⚡ 常用规则模板'),
    h('button', { class: 'primary sm', onclick: () => editRule(null) }, '＋ 新建规则'))));
  const cfg = (await api('GET', '/api/config')).config;
  state.config = cfg;
  const fd = await api('GET', '/api/filters');
  state.filters = fd.filters || [];

  const policies = (cfg.policies || []).slice();
  const body = policies.length === 0
    ? emptyState('⇉', '还没有规则', '', h('button', { class: 'primary sm', onclick: () => editRule(null) }, '新建规则'))
    : (rulesViewMode === 'card' ? rulesCardsView(policies) : rulesTableView(policies));

  // 设备分流模式放在规则列表上方:它决定"流量走不走代理",是规则生效前的总开关。
  root.append(deviceModeCard(cfg));
  root.append(card('', `共 ${policies.length} 条规则;从上往下匹配,先命中的生效。`, '⇉', 'i-blue', body));
}

// 出口名(与 SMBox 卡片"出口"行一致)
function ruleOutboundLabel(p) {
  if (p.as_group || p.target === 'self') return '节点组';
  if (p.target === 'direct') return '直连';
  if (p.target === 'block') return '拒绝';
  return targetLabel(p.target, false, p.name);
}

function ruleOutboundBadge(p) {
  if (p.as_group || p.target === 'self') return 'badge info';
  if (p.target === 'direct') return 'badge ok';
  if (p.target === 'block') return 'badge err';
  return 'badge';
}

// 卡片的操作按钮组:启用/停用、编辑、删除(对齐 SMBox 的四图标)
function ruleActions(p) {
  return h('div', { class: 'row', style: { gap: '6px' } },
    h('button', {
      class: 'sm', title: p.enabled ? '停用' : '启用',
      onclick: async () => {
        try {
          await api('PUT', '/api/policies/' + p.id, Object.assign({}, p, { enabled: !p.enabled }));
          toast(p.enabled ? '已停用' : '已启用', 'ok');
          renderPage();
        } catch (e) { toast(e.message, 'err', 6000); }
      },
    }, p.enabled ? '⏸ 停用' : '▶ 启用'),
    h('button', { class: 'sm', onclick: () => editRule(p) }, '✎ 编辑'),
    h('button', { class: 'sm danger', onclick: () => deleteRule(p) }, '🗑'));
}

function rulesCardsView(policies) {
  return h('div', { class: 'rules-cards' }, ...policies.map((p) => {
    const groups = p.groups || [];
    const nConditions = groups.reduce((n, g) => n + (g.items || []).length, 0)
      || countMatch(p.match);
    const typeNames = groupTypeNames(groups, p.match);
    const modes = [...new Set(groups.map((g) => (g.mode || 'or').toUpperCase()))];
    return h('div', { class: 'rule-card' },
      h('div', { class: 'row', style: { marginBottom: '8px' } },
        h('span', { style: { fontWeight: '600', fontSize: '14.5px' } }, p.name),
        h('span', { class: 'spacer' }),
        p.enabled ? h('span', { class: 'badge ok' }, '启用') : h('span', { class: 'badge' }, '停用')),
      h('div', { class: 'small dim', style: { marginBottom: '8px' } },
        typeNames.join(' · ') + (p.raw_json ? ' · 自定义JSON' : '')),
      h('div', { class: 'rule-kv' }, h('span', {}, '规则数'), h('b', {}, String(nConditions))),
      h('div', { class: 'rule-kv' }, h('span', {}, '匹配模式'),
        h('b', {}, modes.join(' / ') || 'OR')),
      h('div', { class: 'rule-kv' }, h('span', {}, '出口'),
        h('span', { class: ruleOutboundBadge(p) }, ruleOutboundLabel(p))),
      ruleActions(p));
  }));
}

function countMatch(m) {
  if (!m) return 0;
  let n = 0;
  for (const k of Object.keys(m)) {
    if (Array.isArray(m[k])) n += m[k].length ? 1 : 0;
  }
  return n;
}

function groupTypeNames(groups, match) {
  if (groups && groups.length) {
    const seen = [];
    for (const g of groups) {
      for (const c of (g.items || [])) {
        const name = COND_TYPE_NAMES[c.type] || c.type;
        if (!seen.includes(name)) seen.push(name);
      }
    }
    return seen;
  }
  if (match) {
    const map = [['domain_suffix','域名后缀'],['domain','域名'],['domain_keyword','域名关键字'],
      ['ip_cidr','目标IP'],['geo_ip','目标IP'],['geo_site','规则集'],['rule_sets','规则集'],['port','目标端口'],['network','网络类型']];
    const seen = [];
    for (const [k, n] of map) {
      if (match[k] && match[k].length && !seen.includes(n)) seen.push(n);
    }
    if (seen.length) return seen;
  }
  return ['(空)'];
}

function rulesTableView(policies) {
  return h('table', { class: 'desktop-table' },
    h('thead', {}, h('tr', {},
      h('th', {}, '规则'), h('th', {}, '条件'), h('th', {}, '出口'), h('th', {}, '状态'), h('th', {}, '操作'))),
    h('tbody', {}, ...policies.map((p) => h('tr', {},
      h('td', {}, h('div', { style: { fontWeight: '500' } }, p.name,
        p.as_group ? h('span', { class: 'badge pp', style: { marginLeft: '6px' } }, '选线组') : null)),
      h('td', { class: 'small dim', title: ((p.match && p.match.rule_sets) || []).join('\n') },
        p.groups && p.groups.length ? groupSummary(p.groups) : matchText(p.match)),
      h('td', {}, h('span', { class: ruleOutboundBadge(p) }, ruleOutboundLabel(p))),
      h('td', {}, p.enabled ? h('span', { class: 'badge ok' }, '启用') : h('span', { class: 'badge' }, '停用')),
      h('td', {},
        h('button', { class: 'sm', onclick: () => editRule(p) }, '编辑'), ' ',
        h('button', { class: 'sm danger', onclick: () => deleteRule(p) }, '删除'))))));
}

function groupSummary(groups) {
  const parts = (groups || []).map((g) => {
    const items = (g.items || []).map((c) =>
      (COND_TYPE_NAMES[c.type] || c.type) + ':' + (c.values || []).slice(0, 3).join(',') +
      ((c.values || []).length > 3 ? '…' : ''));
    return items.join(g.mode === 'and' ? ' 且 ' : ' 或 ');
  });
  return parts.join('；') || '(空)';
}

// ---------------------------------------------------------------------------
// 规则编辑器(SMBox 同款:名称/出口/多条件组/每组 类型+值+组内OR-AND/组间OR)
// ---------------------------------------------------------------------------

const COND_TYPE_NAMES = {
  rule_set: '规则集', domain_suffix: '域名后缀', domain_keyword: '域名关键字',
  domain: '域名', source_ip_cidr: '源IP', ip_cidr: '目标IP',
  port: '目标端口', port_range: '目标端口范围', network: '网络类型',
};
const COND_PLACEHOLDERS = {
  rule_set: '每行一个 https://... 的 .srs / .json 规则集地址',
  domain_suffix: 'openai.com\nanthropic.com',
  domain_keyword: 'google\nyoutube',
  domain: 'www.example.com',
  source_ip_cidr: '192.168.1.0/24\n(设备的来源 IP)',
  ip_cidr: '1.2.3.0/24 或 geoip:cn',
  port: '443\n80',
  port_range: '6000-6500',
  network: 'tcp 或 udp,可多行',
};

async function editRule(p) {
  const isNew = !p;
  const d = p || { name: '', target: 'self', enabled: true, as_group: true, groups: [], match: {}, raw_json: '' };
  // 旧配置(扁平 Match)在编辑器里转换成条件组,保存后即升级为新格式
  let groups = (d.groups || []).map((g) => ({ mode: g.mode || 'or', items: (g.items || []).map((c) => ({ type: c.type, values: (c.values || []).join('\n') })) }));
  if (!groups.length && !d.raw_json) {
    const flat = flattenMatchToConditions(d.match);
    if (flat.length) groups = [{ mode: 'or', items: flat }];
  }
  if (!groups.length) groups = [{ mode: 'or', items: [] }];

  const name = h('input', { value: d.name, placeholder: '例如:AI / Youtube' });
  const target = h('select', {}, ...targetOptions(d.target));
  const enabled = h('input', { type: 'checkbox', checked: d.enabled !== false });
  const useJson = h('input', { type: 'checkbox', checked: !!d.raw_json });
  const jsonTa = h('textarea', { placeholder: '[{"rule_set":["my-ruleset"],"action":"route","outbound":"Proxy"}]', style: { height: '120px', marginTop: '6px' } }, d.raw_json || '');
  const groupsBox = h('div');
  const msg = h('div', { class: 'small', style: { color: 'var(--red)', minHeight: '20px' } });

  function syncJsonMode() {
    groupsBox.style.display = useJson.checked ? 'none' : '';
    jsonTa.style.display = useJson.checked ? '' : 'none';
  }
  useJson.addEventListener('change', syncJsonMode);

  function renderGroups() {
    clear(groupsBox);
    groups.forEach((g, gi) => {
      const modeBtn = h('button', {
        class: 'sm', title: '点击切换组内匹配模式',
        onclick: () => { g.mode = (g.mode || 'or') === 'or' ? 'and' : 'or'; renderGroups(); },
      }, (g.mode || 'or').toUpperCase());
      const itemsBox = h('div');
      (g.items || []).forEach((c, ci) => {
        itemsBox.append(condRow(g, gi, c, ci, itemsBox));
      });
      const addBtn = h('button', {
        class: 'sm', style: { marginTop: '4px' },
        onclick: () => { (g.items = g.items || []).push({ type: 'domain_suffix', values: '' }); renderGroups(); },
      }, '＋ 添加条件');
      const delGroup = groups.length > 1 ? h('button', {
        class: 'sm danger', title: '删除本条件组',
        onclick: () => { groups.splice(gi, 1); renderGroups(); },
      }, '✕ 删组') : null;
      groupsBox.append(h('div', { class: 'cond-group' },
        h('div', { class: 'row', style: { marginBottom: '8px' } },
          h('span', { class: 'badge info' }, '条件组 ' + (gi + 1)),
          modeBtn,
          h('span', { class: 'small dim' }, (g.mode || 'or') === 'or' ? '组内任一命中' : '组内全部命中'),
          h('span', { class: 'spacer' }), delGroup),
        itemsBox, addBtn));
      if (gi < groups.length - 1) {
        groupsBox.append(h('div', { class: 'or-divider' }, '或'));
      }
    });
    // 「添加条件组」按钮在 content 构建时直接放进插槽;
    // 之前用 document.querySelector 找插槽填充,但首次 rebuild 发生在
    // 弹窗挂进文档之前,插槽还不存在,按钮就永远没出现。
  }

  function condRow(g, gi, c, ci, itemsBox) {
    const typeSel = h('select', {
      style: { width: '140px', flex: 'none' },
      onchange: (e) => { c.type = e.target.value; renderGroups(); },
    }, ...Object.entries(COND_TYPE_NAMES).map(([v, n]) =>
      h('option', { value: v, selected: (c.type || 'domain_suffix') === v }, n)));
    const vals = h('textarea', {
      placeholder: COND_PLACEHOLDERS[c.type] || '', rows: 2,
      style: { flex: 1, minWidth: '180px', minHeight: '44px' },
      oninput: (e) => { c.values = e.target.value; },
    }, c.values || '');
    const del = h('button', {
      class: 'sm danger', style: { flex: 'none' },
      onclick: () => { g.items.splice(ci, 1); if (!g.items.length) g.items.push({ type: 'domain_suffix', values: '' }); renderGroups(); },
    }, '✕');
    return h('div', { class: 'cond-row', key: ci }, typeSel, vals, del);
  }

  let content;
  const rebuild = () => { renderGroups(); syncJsonMode(); };
  content = h('div', {},
    h('div', { class: 'grid g2' },
      field('名称 *', name, '作为选线组时会以内核出站组的名称显示'),
      field('出口', target, '节点组 = 生成同名选线组,可单独指定线路')),
    h('div', { class: 'rule-conditions' },
      h('div', { class: 'row', style: { marginBottom: '10px' } },
        h('span', { class: 'card-title' }, '规则条件'),
        h('span', { class: 'small dim' }, '条件组之间是「或」;组内可选 或/且')),
      groupsBox,
      h('div', { class: 'add-group-slot' }, h('button', {
        class: 'sm', style: { marginTop: '10px' },
        onclick: () => { groups.push({ mode: 'or', items: [] }); renderGroups(); },
      }, '＋ 添加条件组(与上方各条件组为「或」)'))),
    h('div', { class: 'field check', style: { marginTop: '14px' } },
      h('label', {}, useJson, '使用 Sing-box JSON 规则'),
      h('div', { class: 'hint' }, '高级用法:直接提供内核规则数组,忽略上方条件组;保存时由内核校验')),
    jsonTa,
    checkField('启用', enabled),
    msg);

  rebuild();
  openModal(isNew ? '新建规则' : '编辑规则', content, async () => {
    msg.textContent = '';
    if (!name.value.trim()) throw new Error('请填写规则名称');
    let condGroups = [];
    let rawJson = '';
    if (useJson.checked) {
      rawJson = jsonTa.value.trim();
      if (!rawJson) throw new Error('请填写规则 JSON');
      try {
        const arr = JSON.parse(rawJson);
        if (!Array.isArray(arr) || !arr.length) throw new Error('必须是规则对象数组');
      } catch (e) { throw new Error('JSON 无效: ' + e.message); }
    } else {
      for (const g of groups) {
        const items = [];
        for (const c of (g.items || [])) {
          const values = String(c.values || '').split(/[\n,]/).map((x) => x.trim()).filter(Boolean);
          if (!values.length) continue;
          items.push({ type: c.type, values });
        }
        if (items.length) condGroups.push({ mode: g.mode || 'or', items });
      }
      if (!condGroups.length) throw new Error('请至少添加一个有值的条件');
    }
    const isGroupTarget = target.value === 'self' || (target.value.startsWith('group:') || target.value.startsWith('filter:') || target.value.startsWith('chain:') || target.value.startsWith('node:'));
    const body = {
      name: name.value.trim(),
      target: useJson.checked ? 'direct' : target.value,
      enabled: enabled.checked,
      as_group: !useJson.checked && target.value === 'self',
      raw_json: rawJson,
      groups: condGroups,
      match: { domain_suffix: [] }, // 保留字段兼容旧数据;渲染以 groups 优先
    };
    if (isNew) await api('POST', '/api/policies', body);
    else await api('PUT', '/api/policies/' + p.id, body);
    toast('已保存', 'ok');
    closeModal();
    renderPage();
  }, { wide: true });
}

// 删除一条分流规则。
//
// 这里曾经是个"点了没反应"的坑:卡片视图与表格视图的删除按钮都调用
// deleteRule(p),但函数从来没定义过 —— 点击时 ReferenceError 被静默吞掉,
// 表现为"规则删不掉"。后端 DELETE /api/policies/:id 一直是好的。
async function deleteRule(p) {
  if (!p) return;
  if (!p.id) { toast('这条规则没有 ID,无法删除(刷新本页后重试)', 'err', 6000); return; }
  // 选线组(AsGroup)只是给自己生成同名出站:别的规则的出口是 group:/filter:
  // 这类前缀引用,不会引用规则 ID,所以删除不会牵连别的规则,直接确认即可。
  const extra = p.as_group ? '\n(这条会生成同名选线组,删除后内核面板里那个组也会消失)' : '';
  if (!confirm(`确定删除规则「${p.name || p.id}」?${extra}`)) return;
  try {
    await api('DELETE', '/api/policies/' + encodeURIComponent(p.id));
    toast('已删除规则「' + (p.name || p.id) + '」', 'ok');
    await renderPage();
  } catch (e) { toast(e.message, 'err', 6000); }
}

function flattenMatchToConditions(m) {
  if (!m) return [];
  const out = [];
  const add = (t, vals) => { if (vals && vals.length) out.push({ type: t, values: vals.join('\n') }); };
  add('domain', m.domain);
  add('domain_suffix', m.domain_suffix);
  add('domain_keyword', m.domain_keyword);
  const ips = [];
  for (const g of m.geo_ip || []) ips.push('geoip:' + g);
  for (const r of m.ip_cidr || []) ips.push(r);
  add('ip_cidr', ips);
  add('source_ip_cidr', m.client_ip);
  add('port', (m.port || []).map(String));
  add('port_range', m.port_range);
  add('network', m.network);
  add('rule_set', m.rule_sets);
  const sites = [];
  for (const g of m.geo_site || []) sites.push('https://github.com/MetaCubeX/meta-rules-dat/raw/sing/geo/geosite/' + g + '.srs');
  add('rule_set', sites);
  return out;
}
// ---------------------------------------------------------------------------
// 链式代理
// ---------------------------------------------------------------------------

async function renderChains(root) {
  root.append(head('链式代理', h('div', { class: 'row' },
    h('button', { class: 'sm', onclick: probeAllChains }, '检测全部'),
    h('button', { class: 'primary sm', onclick: () => editChain(null) }, '＋ 新建链式代理'))));

  const d = await api('GET', '/api/chains');
  state.chains = d.chains || [];
  state.frontOptions = d.front_options || [];

  let body;
  if (state.chains.length === 0) {
    body = emptyState('⛓', '还没有链式代理', '链式代理 = 前置线路 + 住宅/静态 IP 出口',
      h('button', { class: 'primary sm', onclick: () => editChain(null) }, '创建第一条'));
  } else {
    body = h('div');
    for (const ch of state.chains) body.append(chainCard(ch));
  }
  root.append(card('',
    '给住宅 / 静态 IP 这类出口指定一个前置节点组,流量先经前置再到出口。建好后可当普通节点用。',
    '', '', body));
}

// chainCard 渲染一条链式代理。抽成独立函数是为了控制嵌套深度。
function chainCard(ch) {
  const front = ch.front_group_name || '未设置前置';
  const el = h('div', { class: 'card chain-card', style: { marginBottom: '12px' } });

  const status = ch.disabled
    ? h('span', { class: 'badge warn' }, '已禁用')
    : h('span', { class: 'badge ok' }, h('span', { class: 'dot' }), '启用中');

  el.append(h('div', { class: 'row' },
    h('div', {},
      h('div', { style: { fontWeight: '600', fontSize: '14.5px' } }, ch.name),
      h('div', { class: 'small dim' }, status)),
    h('div', { class: 'spacer' }),
    h('button', { class: 'sm', onclick: () => probeChain(ch.id) }, '检测'),
    h('button', { class: 'sm', onclick: () => editChain(ch) }, '编辑'),
    h('button', { class: 'sm danger', onclick: () => deleteChain(ch) }, '删除')));

  const exitText = ch.type + '://' + ch.host + ':' + ch.port;
  el.append(h('div', { class: 'flow' },
    h('span', { class: 'node' }, '本机'),
    h('span', { class: 'arrow' }, '→'),
    h('span', { class: 'node front' }, '前置:' + front),
    h('span', { class: 'arrow' }, '→'),
    h('span', { class: 'node exit' }, '出口:' + exitText),
    h('span', { class: 'arrow' }, '→'),
    h('span', { class: 'node' }, '互联网')));

  if ((ch.references || []).length) {
    el.append(h('div', { class: 'small dim' }, '被引用:' + ch.references.join('、')));
  }
  if (ch.probe) el.append(probeLine(ch.probe));
  return el;
}

function probeLine(p) {
  const wrap = h('div', { style: { marginTop: '8px' } });
  if (p.delay_ms > 0) {
    const row = h('div', {},
      h('span', { class: 'badge ok' }, '延时 ' + p.delay_ms + ' ms'));
    if (p.exit_ip) {
      row.append(h('span', { style: { marginLeft: '10px', fontSize: '12.5px' } },
        '出口 IP ',
        h('span', { class: 'mono', style: { color: 'var(--green)' } }, p.exit_ip),
        p.country ? h('span', { class: 'dim' }, ' · ' + p.country +
          (p.region ? ' ' + p.region : '') + (p.isp ? ' · ' + p.isp : '')) : null));
    }
    row.append(h('span', { class: 'dim small', style: { marginLeft: '10px' } }, relTime(p.at)));
    wrap.append(row);
  } else {
    wrap.append(h('span', { class: 'badge err' }, '检测失败:' + (p.error || '未知错误')));
  }
  if (p.pool_note) {
    wrap.append(h('div', { class: 'small', style: { color: 'var(--orange)', marginTop: '4px' } }, '⚠ ' + p.pool_note));
  }
  return wrap;
}

async function probeChain(id) {
  const ch = state.chains.find((c) => c.id === id);
  toast('正在检测「' + (ch ? ch.name : id) + '」…', 'info', 2000);
  try {
    const r = await api('POST', '/api/chains/' + id + '/probe');
    const p = r.probe;
    if (p.delay_ms > 0) {
      toast('检测成功:' + p.delay_ms + 'ms' + (p.exit_ip ? ' · 出口 ' + p.exit_ip + (p.country ? ' (' + p.country + ')' : '') : ''), 'ok', 5000);
    } else {
      toast('检测失败:' + (p.error || '未知错误'), 'err', 6000);
    }
    renderPage();
  } catch (e) { toast(e.message, 'err', 6000); }
}

async function probeAllChains() {
  if (!state.chains.length) return;
  toast('正在依次检测所有链式代理…', 'info', 2000);
  for (const c of state.chains) {
    try { await api('POST', '/api/chains/' + c.id + '/probe'); } catch (e) { /* 继续 */ }
  }
  toast('检测完成', 'ok');
  renderPage();
}

async function editChain(ch) {
  const isNew = !ch;
  // 前置组选项必须按"这条链"的视角取:
  //   - 新建:排除含链式代理的组(容易成环)与空组(选了保存不了);
  //   - 编辑:它当前的前置组即使含别的链也必须出现,否则打开编辑框
  //     原选择直接消失,用户保存后前置就丢了。
  let options = state.frontOptions || [];
  try {
    const q = ch ? ('?for=' + encodeURIComponent(ch.id)) : '';
    const d = await api('GET', '/api/chains/options' + q);
    options = d.front_options || [];
  } catch (e) { /* 拉取失败退回缓存 */ }
  const d = ch || {
    name: '', front_group_id: (options[0] || {}).ref || '', type: 'socks5',
    host: '', port: 1080, username: '', password: '', tls_server_name: '',
    insecure: false, udp: false, disabled: false,
  };
  const link = h('input', { placeholder: 'socks5://用户名:密码@地址:端口' });
  const name = h('input', { value: d.name, placeholder: '例如:香港住宅出口' });
  const opts = options.slice();
  if (d.front_group_id && !opts.some((g) => g.ref === d.front_group_id || g.id === d.front_group_id)) {
    // 当前前置组不在候选里(比如它含别的链式代理):保留显示,标注原因,
    // 用户不改就维持原值,改了就是明确的选择。
    opts.unshift({ ref: d.front_group_id, name: d.front_group_name || d.front_group_id, count: 0, current: true });
  }
  const front = h('select', {}, ...opts.map((g) =>
    h('option', { value: g.ref || g.id, selected: (g.ref || g.id) === d.front_group_id },
      (g.kind === 'filter' ? '⛃ ' : '') + g.name +
      (g.current ? '(当前)' : '(' + (g.count || 0) + ' 个节点)'))));
  const type = h('select', {},
    ...[['socks5', 'SOCKS5 (最常见)'], ['http', 'HTTP'], ['https', 'HTTPS (TLS)']].map(([v, n]) =>
      h('option', { value: v, selected: d.type === v }, n)));
  const host = h('input', { value: d.host, placeholder: '例如:192.0.2.10' });
  const port = h('input', { type: 'number', value: d.port || 1080, min: 1, max: 65535 });
  const user = h('input', { value: d.username || '', placeholder: '留空表示无认证' });
  const pass = h('input', { type: 'password', value: d.password || '', placeholder: '留空表示无认证' });
  const sni = h('input', { value: d.tls_server_name || '', placeholder: '留空使用出口地址' });
  const insecure = h('input', { type: 'checkbox', checked: d.insecure });
  const udp = h('input', { type: 'checkbox', checked: d.udp });
  const disabled = h('input', { type: 'checkbox', checked: d.disabled });
  const msg = h('div', { class: 'small', style: { color: 'var(--red)', minHeight: '20px' } });

  const sniField = field('TLS SNI(HTTPS 出口)', sni);
  const insecureField = checkField('忽略证书校验', insecure, '仅在使用自签名证书的 HTTPS 出口时勾选');
  const syncTLS = () => {
    const on = type.value === 'https';
    sniField.style.display = on ? '' : 'none';
    insecureField.style.display = on ? '' : 'none';
  };
  link.addEventListener('input', () => {
    const v = link.value.trim();
    if (!v) return;
    try {
      const u = new URL(v);
      const scheme = u.protocol.replace(':', '');
      if (['socks5', 'http', 'https'].includes(scheme)) type.value = scheme;
      if (u.hostname) host.value = u.hostname;
      if (u.port) port.value = u.port;
      if (u.username) user.value = decodeURIComponent(u.username);
      if (u.password) pass.value = decodeURIComponent(u.password);
      syncTLS();
    } catch (e) { /* 忽略 */ }
  });

  const content = h('div', {},
    field('粘贴出口链接(可选)', link, '支持 socks5:// / http:// / https://,粘贴后自动填写'),
    field('名称 *', name),
    field('前置节点组 *', front, '流量先经这个组当前选中的节点,再连到出口'),
    h('div', { class: 'grid g2' }, field('出口类型 *', type), field('出口端口 *', port)),
    field('出口地址 *', host),
    h('div', { class: 'grid g2' }, field('用户名', user), field('密码', pass)),
    sniField, insecureField,
    h('div', { class: 'grid g2' },
      checkField('支持 UDP', udp, '多数住宅代理不支持 UDP'),
      checkField('禁用此条', disabled)),
    msg);

  if (!options.length) {
    content.prepend(h('div', {
      class: 'card',
      style: { background: 'rgba(245,158,11,.1)', borderColor: 'rgba(245,158,11,.4)', padding: '12px', marginBottom: '14px' },
    },
      h('div', { style: { fontWeight: '600', marginBottom: '4px' } }, '没有可选的前置节点组'),
      h('div', { class: 'small dim' },
        '前置组要求:至少有一个节点、不是动态组、(新建时)不含其它链式代理。',
        '可以先去「节点组」建一个手动选择组并加入节点。'),
      h('button', { class: 'sm', style: { marginTop: '8px' }, onclick: () => { closeModal(); navTo('groups'); } }, '去建节点组')));
  }

  openModal(isNew ? '新建链式代理' : '编辑链式代理', content, async () => {
    msg.textContent = '';
    if (!name.value.trim()) throw new Error('请填写名称');
    if (!front.value) throw new Error('请选择前置节点组');
    const sel = state.frontOptions.find((g) => g.id === front.value);
    if (sel && sel.usable === false) throw new Error('前置组「' + sel.name + '」还没有可用节点');
    if (!host.value.trim()) throw new Error('请填写出口地址');
    const body = {
      name: name.value.trim(), front_group_id: front.value, type: type.value,
      host: host.value.trim(), port: parseInt(port.value, 10) || 0,
      username: user.value, password: pass.value, tls_server_name: sni.value,
      insecure: insecure.checked, udp: udp.checked, disabled: disabled.checked,
    };
    if (isNew) await api('POST', '/api/chains', body);
    else await api('PUT', '/api/chains/' + ch.id, body);
    toast(isNew ? '链式代理已创建' : '已保存', 'ok');
    closeModal();
    renderPage();
  });
  syncTLS();
}

async function deleteChain(ch) {
  const refs = ch.references || [];
  const msg = refs.length
    ? '「' + ch.name + '」正被以下位置引用:\n\n' + refs.join('\n') + '\n\n删除后这些引用会失效。确定删除?'
    : '确定删除链式代理「' + ch.name + '」?';
  if (!confirm(msg)) return;
  try { await api('DELETE', '/api/chains/' + ch.id); toast('已删除', 'ok'); renderPage(); }
  catch (e) { toast(e.message, 'err', 6000); }
}

// ---------------------------------------------------------------------------
// 高级配置
// ---------------------------------------------------------------------------

async function renderAdvanced(root) {
  root.append(head('高级配置'));
  const cfg = (await api('GET', '/api/config')).config;
  state.config = cfg;
  const dash = state.dashboard || await api('GET', '/api/dashboard').catch(function () { return {}; });
  const kver = (dash.kernel && (dash.kernel.version || dash.kernel.api_version)) || '\u2014';

  root.append(h('div', { class: 'grid g2' },
    card('端口与入站', '局域网设备通过这里接入', '⚒', 'i-blue',
      kv('混合代理端口', String((cfg.inbounds || {}).mixed_port || 7890)),
      kv('监听地址', (cfg.inbounds || {}).mixed_listen || '0.0.0.0'),
      kv('允许局域网', (cfg.inbounds || {}).allow_lan ? '是' : '否'),
      kv('TUN 透明代理', (cfg.inbounds || {}).tun ? '已启用' : '未启用'),
      kv('TUN 自动路由', (cfg.inbounds || {}).tun_auto_route ? '已启用' : '未启用'),
      kv('53 端口 DNS 入口', (cfg.inbounds || {}).dns_listen ? '已启用' : '未启用'),
      kv('协议嗅探', (cfg.inbounds || {}).sniff ? '已启用' : '未启用'),
      kv('内核 API 端口', String((cfg.inbounds || {}).clash_api_port || 9090)),
      h('div', { style: { marginTop: '12px' } },
        h('button', { class: 'sm', onclick: () => navTo('settings') }, '去修改'))),

    card('数据管理', '导出 / 导入 / 备份', '⛁', 'i-purple',
      h('div', { class: 'small dim', style: { marginBottom: '12px' } },
        '导出整份配置可迁移到另一台机器;导入前会自动校验,不通过不会覆盖。每次保存都会自动备份。'),
      h('div', { class: 'row' },
        h('button', { class: 'sm', onclick: exportConfig }, '导出配置'),
        h('button', { class: 'sm', onclick: importConfig }, '导入配置'),
        h('button', { class: 'sm', onclick: showBackups }, '备份与恢复'))),

    card('内核维护', '查看配置 / 重启 / 在线升级内核', '◈', 'i-green',
      kv('内核版本', kver),
      kv('节点出站', String(((cfg.nodes || []).filter((n) => !n.disabled)).length)),
      kv('筛选出站', String((cfg.filters || []).filter((f) => f.enabled && !f.count_only).length)),
      kv('链式代理', String((cfg.chains || []).length)),
      kernelUpgradeRow(),
      h('div', { class: 'row', style: { marginTop: '12px' } },
        h('button', { class: 'sm', onclick: showRender }, '查看内核配置'),
        h('button', { class: 'sm', onclick: async () => {
          try { await api('POST', '/api/kernel/restart'); toast('内核已重启', 'ok'); }
          catch (e) { toast(e.message, 'err', 6000); }
        } }, '重启内核'))),

    card('校验', '', '✓', 'i-orange',
      h('div', { class: 'small dim', style: { marginBottom: '12px' } },
        '检查配置中的引用完整性与循环依赖(链式代理前置、规则出口等)。'),
      h('button', { class: 'sm', onclick: validateConfig }, '立即校验'))));
}

async function validateConfig() {
  try {
    const r = await api('GET', '/api/validate');
    const issues = r.issues || [];
    if (!issues.length) toast('配置校验通过,没有发现问题', 'ok', 4000);
    else toast('发现 ' + issues.length + ' 个问题:\n' + issues.join('\n'), 'err', 9000);
  } catch (e) { toast(e.message, 'err'); }
}

async function showRender() {
  try {
    const r = await api('GET', '/api/render');
    const text = JSON.stringify(r.config, null, 2);
    openModal('内核配置 (sing-box)', h('div', {},
      r.last_error ? h('div', { class: 'badge err', style: { marginBottom: '10px' } }, r.last_error) : null,
      h('div', { class: 'small dim', style: { marginBottom: '8px' } }, '这是根据你的配置生成的内核配置,可复制排查问题'),
      h('textarea', { readonly: true, style: { height: '440px' } }, text)), null);
  } catch (e) { toast(e.message, 'err'); }
}

function exportConfig() {
  api('GET', '/api/config').then((d) => {
    const blob = new Blob([JSON.stringify(d.config, null, 2)], { type: 'application/json' });
    const a = h('a', { href: URL.createObjectURL(blob), download: 'boxhub-config.json' });
    a.click();
    URL.revokeObjectURL(a.href);
    toast('已导出', 'ok');
  }).catch((e) => toast(e.message, 'err'));
}

function importConfig() {
  const ta = h('textarea', { placeholder: '粘贴导出的配置 JSON', style: { height: '300px' } });
  const msg = h('div', { class: 'small', style: { color: 'var(--red)' } });
  openModal('导入配置', h('div', {},
    h('div', { class: 'small dim', style: { marginBottom: '8px' } }, '导入会整体替换当前配置,当前配置会自动备份'),
    ta, msg), async () => {
    let cfg;
    try { cfg = JSON.parse(ta.value); } catch (e) { throw new Error('不是合法的 JSON'); }
    await api('PUT', '/api/config', { config: cfg });
    toast('配置已导入', 'ok');
    closeModal();
    renderPage();
  });
}

async function showBackups() {
  try {
    const r = await api('GET', '/api/backups');
    const list = r.backups || [];
    openModal('备份与恢复', h('div', {},
      h('div', { class: 'small dim', style: { marginBottom: '12px' } }, '每次保存前自动备份,保留最近 20 份'),
      list.length === 0 ? emptyState('⛁', '暂无备份')
        : h('div', { style: { maxHeight: '420px', overflow: 'auto' } },
            h('table', {},
              h('thead', {}, h('tr', {}, h('th', {}, '时间'), h('th', {}, '大小'), h('th', {}, ''))),
              h('tbody', {}, ...list.map((b) => h('tr', {},
                h('td', { class: 'small' }, fmtTime(b.at)),
                h('td', { class: 'small dim' }, fmtBytes(b.size)),
                h('td', {}, h('button', { class: 'sm', onclick: async () => {
                  if (!confirm('恢复这个备份?当前配置会被覆盖(当前配置也会自动备份)。')) return;
                  try {
                    await api('POST', '/api/backups/' + encodeURIComponent(b.name));
                    toast('已恢复', 'ok'); closeModal(); renderPage();
                  } catch (e) { toast(e.message, 'err', 8000); }
                } }, '恢复')))))))), null);
  } catch (e) { toast(e.message, 'err'); }
}

// ---------------------------------------------------------------------------
// 内核面板(内嵌)
// ---------------------------------------------------------------------------

function openKernelUINewTab() {
  window.open('/ui/', '_blank');
}

async function renderKernelUI(root) {
  // 先确认内核面板(zashboard)是否已安装
  let ok = true;
  try {
    const r = await fetch('/ui/', { method: 'HEAD' });
    ok = r.ok;
  } catch (e) { ok = false; }

  root.append(head('内核面板', h('div', { class: 'row' },
    h('button', { class: 'sm', onclick: () => renderPage() }, '刷新'),
    h('button', { class: 'sm', onclick: openKernelUINewTab }, '新窗口打开'))));

  if (!ok) {
    root.append(card('', '', '', '', emptyState('▦', '内核面板(zashboard)未安装',
      '安装时下载面板失败(内网访问 GitHub 受限)。可手动补装:把 zashboard 发行包' +
      '(https://github.com/Zephyruso/zashboard/releases/latest/download/dist.zip)' +
      '解压后的 dist/ 内容放到 /opt/boxhub/data/ui/ 后刷新本页;或重跑 bash manage.sh install')));
    return;
  }

  // 入口地址由 /api/kernel-ui-url 生成:同源 /ui/ 加载 zashboard,
  // 后端参数(protocol/hostname/port/secondaryPath/secret)走 query string,
  // API 经面板的 /clash 反代,不涉及跨域,也不用额外放行 9090。
  // 拿不到地址时退回 /ui/ 默认入口(面板会引导用户填后端)。
  let kernelURL = '/ui/';
  let badge = '内核面板(zashboard)';
  try {
    const r = await api('GET', '/api/kernel-ui-url');
    const probe = await fetch(r.url, { method: 'HEAD', mode: 'no-cors' }).then(() => true).catch(() => false);
    if (probe) { kernelURL = r.url; badge = '内核面板(zashboard)'; }
  } catch (e) { /* 回退到 /ui/ 默认入口 */ }
  const wrap = h('div', { class: 'card frame-wrap', style: { padding: 0 } },
    h('div', { class: 'frame-bar' },
      h('span', { class: 'badge info' }, h('span', { class: 'dot' }), badge),
      h('div', { class: 'small dim' }, '实时连接 / 按进程排查 / 手动测速'),
      h('div', { class: 'spacer' }),
      h('button', { class: 'sm', onclick: openKernelUINewTab }, '新窗口')),
    h('iframe', {
      src: kernelURL,
      title: '内核面板',
      sandbox: 'allow-scripts allow-same-origin allow-forms allow-popups',
    }));
  root.append(wrap);
}

// ---------------------------------------------------------------------------
// 日志
// ---------------------------------------------------------------------------

async function renderLogs(root) {
  const level = h('select', { style: { width: 'auto' }, onchange: () => draw() },
    ...[['all', '全部'], ['error', '仅错误'], ['warn', '仅警告'], ['info', '仅信息']].map(([v, n]) =>
      h('option', { value: v }, n)));
  const lines = h('select', { style: { width: 'auto' }, onchange: () => draw() },
    ...[200, 500, 1000, 2000].map((v) => h('option', { value: String(v), selected: v === 500 }, v + ' 行')));

  root.append(head('日志', h('div', { class: 'row' }, level, lines,
    h('button', { class: 'sm', onclick: () => draw() }, '刷新'),
    h('button', { class: 'sm danger', onclick: async () => {
      try { await api('POST', '/api/kernel/log/clear'); toast('日志已清空', 'ok'); draw(); }
      catch (e) { toast(e.message, 'err'); }
    } }, '清空'))));

  const pre = h('pre', { class: 'log' }, '加载中…');
  root.append(h('div', { class: 'card', style: { padding: 0 } }, h('div', { style: { padding: '14px' } }, pre)));

  async function draw() {
    try {
      const r = await api('GET', '/api/logs?lines=' + lines.value + '&level=' + level.value);
      const arr = r.lines || [];
      if (!arr.length) { pre.textContent = '(没有日志)'; return; }
      // 高亮 error / warn 行
      pre.innerHTML = '';
      for (const l of arr) {
        const cls = /\b(FATAL|ERROR)\b/i.test(l) ? 'e' : /\bWARN\b/i.test(l) ? 'w' : '';
        pre.append(h('div', { class: cls }, l));
      }
      pre.scrollTop = pre.scrollHeight;
    } catch (e) {
      pre.textContent = '读取日志失败: ' + e.message;
    }
  }
  await draw();
  state.timer = setInterval(() => { if (state.page === 'logs') draw(); }, 5000);
}

// ---------------------------------------------------------------------------
// 设置
// ---------------------------------------------------------------------------

async function renderSettings(root) {
  root.append(head('设置'));
  const cfg = (await api('GET', '/api/config')).config;
  state.config = cfg;
  const ib = cfg.inbounds || {};
  const dns = cfg.dns || {};

  const f = {
    mixed: h('input', { type: 'checkbox', checked: ib.mixed !== false }),
    mixedPort: h('input', { type: 'number', value: ib.mixed_port || 7890 }),
    mixedListen: h('input', { value: ib.mixed_listen || '0.0.0.0' }),
    allowLan: h('input', { type: 'checkbox', checked: ib.allow_lan !== false }),
    clashPort: h('input', { type: 'number', value: ib.clash_api_port || 9090 }),
    tun: h('input', { type: 'checkbox', checked: !!ib.tun }),
    tunAutoRoute: h('input', { type: 'checkbox', checked: !!ib.tun_auto_route }),
    dnsListen: h('input', { type: 'checkbox', checked: !!ib.dns_listen }),
    sniff: h('input', { type: 'checkbox', checked: !!ib.sniff }),
    blockQuic: h('input', { type: 'checkbox', checked: !!ib.block_quic }),
    dnsDirect: h('input', { value: (dns.direct_servers || []).join(',') }),
    dnsProxy: h('input', { value: (dns.proxy_servers || []).join(',') }),
    fakeip: h('input', { type: 'checkbox', checked: dns.fake_ip !== false }),
    strategy: h('select', {},
      ...[['prefer_ipv4', '优先 IPv4'], ['ipv4_only', '仅 IPv4'], ['prefer_ipv6', '优先 IPv6'], ['ipv6_only', '仅 IPv6']].map(([v, n]) =>
        h('option', { value: v, selected: (dns.strategy || 'prefer_ipv4') === v }, n))),
  };

  root.append(h('div', { class: 'grid g2' },
    card('入站与端口', '', '⚒', 'i-blue',
      checkField('启用混合代理端口', f.mixed, '同时支持 HTTP 与 SOCKS5'),
      h('div', { class: 'grid g2' }, field('混合端口', f.mixedPort), field('监听地址', f.mixedListen)),
      checkField('允许局域网访问', f.allowLan, '关闭后只监听本机'),
      checkField('启用 TUN 透明代理', f.tun, '容器内需要额外放行 /dev/net/tun'),
      checkField('TUN 自动路由 (auto_route)', f.tunAutoRoute, '网关模式必需:接管默认路由并劫持 53 端口 DNS,全网设备把网关与 DNS 指向本机即可分流'),
      checkField('监听 53 端口 (DNS 入口)', f.dnsListen, '局域网设备把 DNS 指向本机即可分流,无需 dnsmasq;端口被占用时自动跳过'),
      checkField('协议嗅探 (sniff)', f.sniff, 'TUN 网关场景必需:从流量中嗅探域名,否则域名分流规则对局域网设备失效'),
      checkField('屏蔽走代理的 QUIC', f.blockQuic, '多数节点 UDP 转发差,开启后浏览器退回 TCP'),
      field('内核 API 端口', f.clashPort)),

    card('域名解析 (DNS)', '', '◎', 'i-purple',
      field('直连 DNS', f.dnsDirect, '中国大陆域名用,逗号分隔'),
      field('代理 DNS', f.dnsProxy, '境外域名用,支持 udp:// tcp:// tls:// https://'),
      field('解析策略', f.strategy),
      checkField('启用 FakeIP', f.fakeip, '走代理的域名返回占位地址,直连域名真实解析'))));

  root.append(h('div', { class: 'row' },
    h('button', { class: 'primary', onclick: async (e) => {
      const b = e.currentTarget; b.disabled = true;
      try {
        await api('PUT', '/api/settings', {
          inbounds: {
            mixed: f.mixed.checked,
            mixed_port: parseInt(f.mixedPort.value, 10) || 0,
            mixed_listen: f.mixedListen.value.trim(),
            allow_lan: f.allowLan.checked,
            clash_api_port: parseInt(f.clashPort.value, 10) || 0,
            tun: f.tun.checked,
            tun_auto_route: f.tunAutoRoute.checked,
            dns_listen: f.dnsListen.checked,
            sniff: f.sniff.checked,
            block_quic: f.blockQuic.checked,
            clash_api: true,
          },
          dns: {
            direct_servers: f.dnsDirect.value.split(/[,，]/).map((s) => s.trim()).filter(Boolean),
            proxy_servers: f.dnsProxy.value.split(/[,，]/).map((s) => s.trim()).filter(Boolean),
            fake_ip: f.fakeip.checked,
            strategy: f.strategy.value,
            hijack: dns.hijack !== false,
          },
        });
        toast('设置已保存并生效', 'ok');
      } catch (err) { toast(err.message, 'err', 8000); }
      b.disabled = false;
    } }, '保存设置')));
}

// ---------------------------------------------------------------------------
// 弹窗
// ---------------------------------------------------------------------------

let modalEl = null;

function openModal(title, content, onOk, opts) {
  closeModal();
  opts = opts || {};
  const mask = h('div', { class: 'mask', onclick: (e) => { if (e.target === mask) closeModal(); } });
  const actions = onOk
    ? h('div', { class: 'modal-actions' },
        h('button', { onclick: closeModal }, '取消'),
        h('button', { class: 'primary', onclick: async (e) => {
          const b = e.currentTarget; b.disabled = true;
          try { await onOk(); } catch (err) {
            const m = $$('.small', modalEl).find((x) => x.style.color.includes('red') || x.style.color.includes('--red'));
            if (m) m.textContent = err.message; else toast(err.message, 'err', 8000);
          }
          b.disabled = false;
        } }, '保存'))
    : h('div', { class: 'modal-actions' }, h('button', { onclick: closeModal }, '关闭'));
  const modal = h('div', { class: 'modal' + (opts.wide ? ' wide' : '') }, h('h3', {}, title), content, actions);
  mask.append(modal);
  document.body.append(mask);
  modalEl = mask;
}

function closeModal() {
  if (modalEl) { modalEl.remove(); modalEl = null; }
}

document.addEventListener('keydown', (e) => { if (e.key === 'Escape') closeModal(); });

// ---------------------------------------------------------------------------
// 帮助
// ---------------------------------------------------------------------------

function showHelp() {
  openModal('帮助 / 关于', h('div', {},
    h('div', { style: { marginBottom: '14px' } },
      h('div', { style: { fontWeight: '600', marginBottom: '4px' } }, 'BoxHub'),
      h('div', { class: 'small dim' }, '开源自托管的代理控制台,基于 sing-box 内核')),
    kv('面板版本', '1.2.0'),
    kv('内核', (state.dashboard && state.dashboard.kernel && state.dashboard.kernel.version) || '—'),
    h('div', { style: { marginTop: '16px' } },
      h('div', { style: { fontWeight: '600', marginBottom: '8px' } }, '快速上手'),
      h('ol', { class: 'small dim', style: { paddingLeft: '20px', lineHeight: '1.9' } },
        h('li', {}, '「节点」添加订阅或粘贴分享链接'),
        h('li', {}, '「筛选」按地区/关键词自动归类节点(可一键生成)'),
        h('li', {}, '「链式代理」给住宅 IP 出口指定前置线路'),
        h('li', {}, '「规则」把指定站点分流到筛选或链式代理'),
        h('li', {}, '局域网设备填 服务器IP:7890 即可使用'))),
  ), null);
}

// ---------------------------------------------------------------------------
// 常用规则模板
// ---------------------------------------------------------------------------

async function showRuleTemplates() {
  let data;
  try {
    data = await api('GET', '/api/rule-templates');
  } catch (e) { toast(e.message, 'err', 6000); return; }

  const tpls = data.templates || [];
  const checks = {};
  const list = h('div', { style: { maxHeight: '460px', overflowY: 'auto' } });

  for (const t of tpls) {
    const cb = h('input', { type: 'checkbox', checked: !t.added && t.default });
    checks[t.key] = cb;
    const tag = t.added
      ? h('span', { class: 'badge', style: { marginLeft: '8px' } }, '已添加')
      : t.target === 'direct'
        ? h('span', { class: 'badge ok', style: { marginLeft: '8px' } }, '直连')
        : h('span', { class: 'badge pp', style: { marginLeft: '8px' } }, '走代理');
    list.append(h('label', {
      class: 'filter-item', style: { marginBottom: '8px', cursor: t.added ? 'default' : 'pointer', opacity: t.added ? '.55' : '1' },
    },
      h('div', { style: { display: 'flex', alignItems: 'center', gap: '10px', minWidth: 0 } },
        cb,
        h('div', { style: { minWidth: 0 } },
          h('div', { style: { fontWeight: '500' } }, t.name, tag),
          h('div', { class: 'small dim' }, t.desc))),
      h('span', { class: 'cnt' }, (t.rule_sets || []).length + ' 个规则集')));
    if (t.added) cb.disabled = true;
  }

  openModal('常用规则模板',
    h('div', {},
      h('div', { class: 'small dim', style: { marginBottom: '14px' } },
        '与 SMBox 相同的规则集来源(MetaCubeX meta-rules-dat)。选中的规则会生成同名选线组,可在内核面板里单独指定线路。'),
      h('div', { class: 'row', style: { marginBottom: '10px' } },
        h('button', {
          class: 'sm',
          onclick: () => {
            for (const t of tpls) if (!t.added) checks[t.key].checked = true;
          },
        }, '全选未添加'),
        h('button', {
          class: 'sm',
          onclick: () => { for (const t of tpls) checks[t.key].checked = false; },
        }, '全不选')),
      list), async () => {
    const keys = tpls.filter((t) => !t.added && checks[t.key].checked).map((t) => t.key);
    if (!keys.length) throw new Error('请至少选择一个未添加的规则');
    const r = await api('POST', '/api/rule-templates/apply', { keys });
    closeModal();
    const added = (r.added || []).length;
    const skipped = (r.skipped || []).length;
    toast('已添加 ' + added + ' 条规则' + (skipped ? '(跳过 ' + skipped + ' 条已存在)' : ''), 'ok', 4000);
    renderPage();
  });
}

// ---------------------------------------------------------------------------
// 内核版本检测与在线升级
// ---------------------------------------------------------------------------

function kernelUpgradeRow() {
  const mirror = h('input', { placeholder: 'GitHub 镜像前缀(可选,如 https://gh-proxy.xx)', value: localStorage.getItem('boxhub.ghMirror') || '' });
  const status = h('div', { class: 'small dim', style: { marginTop: '10px', lineHeight: '1.7' } }, '版本检测走 GitHub 官方 release;不通可填镜像。');
  const checkBtn = h('button', { class: 'sm', style: { marginTop: '10px' } }, '检测更新');
  const upBtn = h('button', { class: 'sm primary', style: { marginTop: '10px', display: 'none' } }, '升级内核');

  checkBtn.addEventListener('click', async () => {
    localStorage.setItem('boxhub.ghMirror', mirror.value.trim());
    checkBtn.disabled = true;
    status.textContent = '正在检测…';
    try {
      const q = mirror.value.trim() ? ('?mirror=' + encodeURIComponent(mirror.value.trim())) : '';
      const r = await api('GET', '/api/kernel/update' + q);
      const line = '当前 ' + (r.current || '?') + ' → 最新 ' + (r.latest || '?') + '(' + r.platform + ')';
      if (r.available) {
        status.replaceChildren(
          h('span', { style: { color: 'var(--orange)', fontWeight: 600 } }, '发现新版本 ' + r.latest + ' '),
          h('span', { class: 'dim' }, line));
        upBtn.style.display = '';
      } else if (r.asset_ok === false) {
        status.textContent = line + ' —— release 里没有当前平台的安装包';
      } else {
        status.textContent = line + ',已是最新' + (r.cached ? '(缓存)' : '');
        upBtn.style.display = 'none';
      }
    } catch (e) {
      status.textContent = '检测失败: ' + e.message;
    }
    checkBtn.disabled = false;
  });

  upBtn.addEventListener('click', async () => {
    if (!confirm('升级 sing-box 内核?\n下载完成后会替换二进制并重启内核(连接短暂中断),失败自动回滚旧版本。')) return;
    upBtn.disabled = true; checkBtn.disabled = true;
    status.textContent = '升级已开始…';
    let timer = setInterval(async () => {
      try {
        const st = await api('GET', '/api/kernel/upgrade/status');
        if (!st.stage) return;
        let line = st.stage;
        // 下载阶段带字节进度:慢网络下能分辨"在爬"还是"卡死"
        if (/download|\u4e0b\u8f7d/.test(st.stage) && st.downloaded > 0) {
          const mb = (n) => (n / 1048576).toFixed(1) + ' MB';
          line += ' — ' + mb(st.downloaded) + (st.total > 0 ? ' / ' + mb(st.total) : '') + (st.total > 0 ? ' (' + Math.floor(st.downloaded / st.total * 100) + '%)' : '');
        }
        status.textContent = line + '…';
      } catch (e) { /* 忽略轮询失败 */ }
    }, 1500);
    try {
      const r = await api('POST', '/api/kernel/upgrade', { mirror: mirror.value.trim() });
      status.replaceChildren(h('span', { style: { color: 'var(--green)', fontWeight: 600 } },
        '升级成功: ' + (r.from || '?') + ' → ' + (r.to || '?') + ',内核已重启'));
      toast('内核已升级到 ' + (r.to || ''), 'ok', 6000);
      setTimeout(async () => { try { await api('GET', '/api/dashboard'); } catch (e) {} location.reload(); }, 2500);
    } catch (e) {
      status.textContent = '升级失败: ' + e.message;
      toast('升级失败(已自动回滚时旧内核照常运行)', 'err', 8000);
    }
    clearInterval(timer);
    upBtn.disabled = false; checkBtn.disabled = false;
  });

  return h('div', { style: { marginTop: '12px' } },
    mirror, h('div', { class: 'row', style: { gap: '8px' } }, checkBtn, upBtn), status);
}

