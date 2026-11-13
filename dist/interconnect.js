'use strict';

(() => {
  const path = '/api/cluster/interconnect';
  const key = e => e.node + '/' + e.container;
  const ref = e => ({node: e.node, container: e.container});
  const dirNames = {ab: 'A → B', ba: 'B → A', both: 'A ↔ B'};
  const panels = {build: 'linkBuild', connections: 'linkSaved', network: 'linkNetwork'};
  let view = null, catalog = new Map(), nodes = new Map();
  let pair = {a: '', b: ''}, side = 'a', editing = '', baseRevision = null;
  let busy = false, loading = false, generation = 0, readSequence = 0;
  let probeID = '', probeSequence = 0;

  function el(tag, cls, content) {
    const element = document.createElement(tag);
    if (cls) element.className = cls;
    if (content !== undefined) element.textContent = content;
    return element;
  }

  function icon(name) {
    const ns = 'http://www.w3.org/2000/svg';
    const svg = document.createElementNS(ns, 'svg'), use = document.createElementNS(ns, 'use');
    svg.setAttribute('class', 'ui-icon');
    svg.setAttribute('aria-hidden', 'true');
    use.setAttribute('href', '#' + name);
    svg.append(use);
    return svg;
  }

  function action(label, run, cls) {
    const button = el('button', cls, label);
    button.type = 'button';
    button.onclick = run;
    return button;
  }

  function setTab(name) {
    for (const [tab, id] of Object.entries(panels)) {
      $(id).hidden = tab !== name;
      const button = document.querySelector('[data-link-tab="' + tab + '"]');
      if (tab === name) button.setAttribute('aria-current', 'page');
      else button.removeAttribute('aria-current');
    }
  }

  function message(text = '', tone = '') {
    $('linkMessage').textContent = text;
    $('linkMessage').dataset.tone = tone;
  }

  function badge(text, tone) {
    return el('span', 'ic-badge ' + tone, text);
  }

  function empty(title, hint) {
    const box = el('div', 'ic-empty');
    box.append(icon('ic-box'), el('strong', '', title), el('p', '', hint));
    return box;
  }

  function unavailable(e) {
    if (!e) return '容器已不可用';
    if (e.removing) return '正在退出';
    if (!e.ip && nodes.get(e.node)?.capability?.error) return '节点未就绪';
    return '';
  }

  function owner(e) { return e.owner || '未归属'; }
  function nodeName(id) { return nodes.get(id)?.name || id; }
  function identity(e) { return [nodeName(e.node), e.name].join(' / '); }
  function labelOf(r) { return catalog.get(key(r))?.name || r.container.slice(0, 12); }

  // Both the connection composer and standalone attachment use the same inventory.
  function picker(id, attachOnly) {
    const root = $(id);
    const controls = el('div', 'ic-filterbar'), searchBox = el('label', 'ic-search');
    const search = el('input'), filter = el('select'), list = el('div', 'ic-items');
    search.type = 'search';
    search.placeholder = '搜索名称、使用者、IP 或 ID';
    search.setAttribute('aria-label', '搜索容器');
    filter.setAttribute('aria-label', '筛选节点');
    searchBox.append(icon('ic-search'), search);
    controls.append(searchBox, filter);
    root.append(controls, list);
    list.setAttribute('aria-label', '容器列表');
    function sync() {
      const last = filter.value;
      filter.replaceChildren(new Option('全部节点', 'all'), ...Array.from(nodes.values(), n => new Option(n.name, n.id)));
      filter.value = nodes.has(last) ? last : 'all';
      draw();
    }
    function draw() {
      const tokens = search.value.trim().toLocaleLowerCase().split(/\s+/);
      const groups = new Map();
      let total = 0;
      for (const e of catalog.values()) {
        if ((attachOnly && e.ip) || (filter.value !== 'all' && e.node !== filter.value)) continue;
        const words = [e.name, owner(e), e.ip, e.container, nodeName(e.node)].join(' ').toLocaleLowerCase();
        if (!tokens.every(t => words.includes(t))) continue;
        if (!groups.has(e.node)) groups.set(e.node, []);
        groups.get(e.node).push(e);
        total++;
      }
      const fragment = document.createDocumentFragment();
      for (const n of nodes.values()) {
        const items = groups.get(n.id);
        if (!items) continue;
        const group = el('div', 'ic-group'), header = el('div', 'ic-group-head');
        header.append(icon('icon-storage'), el('strong', '', n.name), el('span', '', items.length + ' 个'));
        group.append(header);
        for (const e of items.sort((a, b) => a.name.localeCompare(b.name, 'zh-CN') || a.container.localeCompare(b.container))) {
          const value = key(e);
          const selected = !attachOnly && (value === pair.a ? 'a' : value === pair.b ? 'b' : '');
          const row = el('button', 'ic-choice'), glyph = el('span', 'ic-glyph'), copy = el('span', 'ic-identity');
          row.type = 'button';
          row.dataset.key = value;
          if (selected) row.dataset.side = selected;
          const reason = unavailable(e);
          row.disabled = busy || !!reason || (!attachOnly && !!editing);
          glyph.append(icon('ic-box'));
          copy.append(el('strong', '', e.name), el('small', '', owner(e) + ' · ' + (e.ip || e.container.slice(0, 12))));
          const mark = el('span', 'ic-pick-mark', selected ? selected.toUpperCase() : '＋');
          row.append(glyph, copy, badge(reason || (e.ip ? '已接入' : '可接入'), reason ? 'warn' : e.ip ? 'good' : 'quiet'), mark);
          row.title = identity(e) + ' · ' + owner(e) + ' · ' + e.container;
          if (!attachOnly) row.setAttribute('aria-pressed', String(!!selected));
          row.onclick = () => attachOnly ? attach(e) : pick(e);
          group.append(row);
        }
        fragment.append(group);
      }
      if (!total) fragment.append(search.value || filter.value !== 'all'
        ? empty('没有匹配的容器', '试试其他关键词，或切换节点。')
        : empty(attachOnly ? '暂无可接入容器' : '暂无容器', '已接管的 bridge 容器会显示在这里。可在「节点与接入」查看节点状态。'));
      list.replaceChildren(fragment);
      if (!attachOnly) $('linkDirectoryCount').textContent = total + ' 个';
    }
    search.oninput = filter.onchange = draw;
    return {draw, search, sync, clear() { search.value = ''; filter.value = 'all'; draw(); }};
  }

  const directory = picker('linkPicker', false), attachPicker = picker('linkAttachPicker', true);

  function focusPicker() {
    directory.search.focus();
    if (matchMedia('(max-width: 760px)').matches) directory.search.scrollIntoView({block: 'center'});
  }

  function pick(e) {
    if (editing || busy || unavailable(e)) return;
    message();
    const value = key(e);
    if (pair.a === value || pair.b === value) {
      const selected = pair.a === value ? 'a' : 'b';
      pair[selected] = '';
      side = selected;
    } else {
      pair[side] = value;
      if (!pair.a || !pair.b) side = pair.a ? 'b' : 'a';
    }
    drawDraft();
    const row = $('linkPicker').querySelector('[data-key="' + value + '"]');
    if (pair.a && pair.b && matchMedia('(max-width: 760px)').matches) {
      $('linkAddRule').focus({preventScroll: true});
      $('linkEditor').scrollIntoView({block: 'start'});
    } else row?.focus({preventScroll: true});
  }

  function drawSlot(s) {
    const slot = $(s === 'a' ? 'linkA' : 'linkB'), value = pair[s], e = catalog.get(value);
    const letter = s.toUpperCase();
    const select = action('', () => { side = s; drawDraft(); focusPicker(); }, 'ic-slot-select');
    select.disabled = !!editing || busy;
    select.setAttribute('aria-label', '选择端点 ' + letter + (e ? '，当前为 ' + identity(e) : ''));
    select.append(el('span', 'ic-letter', letter), el('strong', '', e?.name || (value ? '容器已不可用' : '选择容器')),
      el('small', '', e ? nodeName(e.node) + ' · ' + (e.ip || '待接入') : '从容器目录中点选'));
    slot.dataset.active = String(!editing && side === s);
    slot.dataset.filled = String(!!value);
    slot.replaceChildren(select);
    if (value && !editing) {
      const clear = action('×', () => { pair[s] = ''; side = s; drawDraft(); focusPicker(); }, 'ic-slot-clear');
      clear.setAttribute('aria-label', '移除端点 ' + letter);
      clear.disabled = busy;
      slot.append(clear);
    }
  }

  function drawDraft(redrawDirectory = true) {
    drawSlot('a');
    drawSlot('b');
    if (redrawDirectory) directory.draw();
    const endpoints = [catalog.get(pair.a), catalog.get(pair.b)];
    const ready = pair.a && pair.b && endpoints.every(e => !unavailable(e));
    const pending = endpoints.filter(e => e && !e.ip).length;
    const current = view?.connections.find(c => c.id === editing && !c.removing);
    const conflict = !!editing && (!current || baseRevision !== view?.revision);
    $('linkPickHint').textContent = editing ? '已保存连接的两端已固定。' : '点选容器填入端点 ' + side.toUpperCase() + '，再次点选可移除。';
    $('linkSave').disabled = busy || !ready || (!!editing && !current);
    $('linkSave').textContent = pending ? '接入并保存连接' : '保存并应用';
    $('linkSaveHint').textContent = !ready ? '请从目录选择两个不同的可用容器。' :
      pending ? '保存时将先为 ' + pending + ' 个容器分配专用 IPv4 地址，再应用规则。' : '';
    $('linkEditorTitle').textContent = editing ? '编辑连接' : '新建连接';
    $('linkNew').hidden = !editing;
    $('linkConflict').hidden = !conflict;
    $('linkEditorVersion').textContent = conflict ? (current ? '配置已更新。草稿基于版本 ' + baseRevision + '，可重载最新规则。' : '这条连接已被删除或正在撤销。') : '';
    $('linkReload').hidden = !current;
  }

  function rule(r = {direction: 'ab', protocol: 'tcp', first: 0, last: 0, enabled: true}) {
    const row = el('div', 'ic-rule');
    row.innerHTML = '<label>方向<select name="direction"><option value="ab">A → B</option><option value="ba">B → A</option><option value="both">双向</option></select></label>' +
      '<label>协议<select name="protocol"><option value="tcp">TCP</option><option value="udp">UDP</option><option value="icmp">ICMP</option><option value="all">全部 IP</option></select></label>' +
      '<label>起始目的端口<input name="first" type="number" min="1" max="65535" placeholder="全部"></label>' +
      '<label>结束目的端口<input name="last" type="number" min="1" max="65535" placeholder="同起始"></label>' +
      '<div class="ic-rule-foot"><label class="ic-check"><input name="enabled" type="checkbox">启用规则</label></div>';
    const {direction, protocol, first, last, enabled} = Object.fromEntries(Array.from(row.querySelectorAll('[name]'), f => [f.name, f]));
    direction.value = r.direction;
    protocol.value = r.protocol;
    first.value = r.first || '';
    last.value = r.last || '';
    enabled.checked = r.enabled;
    function ports() {
      first.disabled = last.disabled = !['tcp', 'udp'].includes(protocol.value);
      last.min = first.value || '1';
      last.setCustomValidity(!last.disabled && last.value && !first.value ? '请先填写起始目的端口。' : '');
    }
    protocol.onchange = first.oninput = last.oninput = ports;
    ports();
    row.lastElementChild.append(action('移除', () => { row.remove(); $('linkAddRule').focus(); }, 'ic-text'));
    $('linkRules').append(row);
  }

  function startDraft(c) {
    editing = c?.id || '';
    baseRevision = c ? view.revision : null;
    pair = {a: c ? key(c.a) : '', b: c ? key(c.b) : ''};
    side = 'a';
    $('linkRules').replaceChildren();
    if (c) c.rules.forEach(rule);
    else rule();
    $('linkEnabled').checked = c?.enabled ?? true;
    drawDraft();
  }

  function state(c) {
    if (c.removing) return badge('待撤销', 'warn');
    const sides = [nodes.get(c.a.node), nodes.get(c.b.node)];
    const failed = n => !n || n.error || n.capability?.error;
    const applied = n => !failed(n) && n.applied >= c.revision;
    if (sides.every(applied)) return badge('已应用', 'good');
    if (sides.some(failed)) return badge(sides.some(applied) ? '部分失败' : '应用失败', 'warn');
    return badge('待应用', 'quiet');
  }

  function ruleText(r) {
    const ports = ['tcp', 'udp'].includes(r.protocol) ? ' · ' + (r.first ? r.first + (r.last !== r.first ? '–' + r.last : '') : '全部端口') : '';
    return dirNames[r.direction] + ' · ' + (r.protocol === 'all' ? '全部 IP' : r.protocol.toUpperCase()) + ports + (r.enabled ? '' : ' · 已禁用');
  }

  function connectionRow(c) {
    const row = el('article', 'ic-saved-row'), header = el('div', 'ic-row-head'), route = el('div', 'ic-saved-route');
    for (const s of ['a', 'b']) {
      if (s === 'b') route.append(icon('ic-link'));
      const end = el('div', 'ic-saved-end'), e = catalog.get(key(c[s]));
      end.dataset.side = s;
      const copy = el('div', 'ic-identity');
      copy.append(el('strong', '', labelOf(c[s])), el('small', '', [nodeName(c[s].node), e?.owner, e?.ip].filter(Boolean).join(' · ')));
      end.append(el('span', 'ic-letter', s.toUpperCase()), copy);
      route.append(end);
    }
    header.append(route, badge(c.enabled ? '已启用' : '已禁用', 'quiet'), state(c));
    const rules = el('div', 'ic-rule-badges');
    for (const r of c.rules) rules.append(el('span', '', ruleText(r)));
    if (!c.rules.length) rules.textContent = '无允许规则';
    const footer = el('div', 'ic-row-foot'), actions = el('div', 'ic-actions');
    footer.append(el('small', 'ic-revision', '配置 v' + c.revision + ' · A v' + nodes.get(c.a.node)?.applied + ' / B v' + nodes.get(c.b.node)?.applied), actions);
    if (!c.removing) {
      actions.append(action('编辑', () => { startDraft(c); setTab('build'); $('linkAddRule').focus(); }));
      if (c.enabled) actions.append(action('诊断', () => openProbe(c)), action('禁用', () => mutate({action: 'disable', id: c.id})));
      actions.append(action('删除', () => {
        if (window.confirm('删除 ' + identity(catalog.get(key(c.a))) + ' 与 ' + identity(catalog.get(key(c.b))) + ' 的连接和访问规则？')) mutate({action: 'delete', id: c.id});
      }, 'ic-text'));
    }
    row.append(header, rules, footer);
    return row;
  }

  function drawNetwork() {
    const {connections, endpoints} = view;
    $('linkConnections').replaceChildren(...connections.map(connectionRow));
    if (!connections.length) $('linkConnections').append(empty('尚无连接', '选择「新建连接」，从目录中选出两个容器。'));
    $('linkNodes').replaceChildren(...view.nodes.map(n => {
      const status = n.capability || {}, row = el('article', 'ic-record'), copy = el('div', 'ic-identity');
      copy.append(el('strong', '', n.name), el('small', '', [n.internal_ip, status.interface || '待检查接口', n.subnet || '待分配 /24',
        status.protocol ? 'helper v' + status.protocol : 'helper 未就绪', status.firewall].filter(Boolean).join(' · ')));
      row.append(icon('icon-storage'), copy, el('small', 'ic-revision', '配置 v' + n.desired + ' / 已应用 v' + n.applied));
      const errors = [...new Set([n.error, status.error].filter(Boolean))];
      if (errors.length) row.append(el('p', 'ic-node-error', errors.join(' · ')));
      return row;
    }));
    if (!view.nodes.length) $('linkNodes').append(empty('尚无节点', '在集群总控添加 worker 后即可检查节点。'));
    $('linkEndpoints').replaceChildren(...endpoints.map(e => {
      const row = el('article', 'ic-record'), copy = el('div', 'ic-identity');
      const br = 'alb' + e.ip.split('.').map(x => Number(x).toString(16).padStart(2, '0')).join('');
      copy.append(el('strong', '', e.name), el('small', '', [nodeName(e.node), owner(e), e.ip, br].join(' · ')));
      row.append(icon('ic-box'), copy);
      if (e.removing) row.append(badge('待撤销', 'warn'));
      else if (connections.some(c => key(c.a) === key(e) || key(c.b) === key(e))) row.append(el('small', '', '需先删除关联连接'));
      else row.append(action('退出', () => {
        if (window.confirm('退出 ' + identity(e) + ' 并释放地址 ' + e.ip + '？')) mutate({action: 'detach', endpoint: ref(e)});
      }));
      return row;
    }));
    if (!endpoints.length) $('linkEndpoints').append(empty('尚无接入容器', '可单独接入容器，也可在新建连接时一起接入。'));
    $('linkPool').value = view.pool;
    $('linkPool').disabled = $('linkPoolForm').querySelector('button').disabled = !!endpoints.length;
    $('linkPoolHint').textContent = endpoints.length ? '已有容器接入，地址池已锁定。' : '在接入第一个容器前可以更改。';
  }

  function render(v) {
    if (view?.revision !== v.revision && $('linkProbeDialog').open) $('linkProbeDialog').close();
    view = v;
    nodes = new Map(v.nodes.map(n => [n.id, n]));
    catalog = new Map(v.endpoints.map(e => [key(e), e]));
    for (const n of v.nodes) {
      for (const c of n.candidates || []) {
        const e = {node: n.id, container: c.id, name: c.name, owner: c.owner};
        if (!catalog.has(key(e))) catalog.set(key(e), e);
      }
    }
    $('linkNodeCount').textContent = v.nodes.length;
    $('linkEndpointCount').textContent = v.endpoints.length;
    $('linkConnectionCount').textContent = v.connections.length;
    $('linkHeroPool').textContent = v.pool;
    $('linkVersion').textContent = '配置版本 ' + v.revision;
    directory.sync();
    attachPicker.sync();
    drawNetwork();
    drawDraft(false);
  }

  function lock(state) {
    busy = state;
    for (const id of ['linkEditor', 'linkPicker', 'linkSaved', 'linkNetwork', 'linkAttachPicker']) $(id).inert = state;
    $('linkRefresh').disabled = state || loading;
    drawDraft();
    attachPicker.draw();
  }

  // Keep every acknowledged attachment visible if the next operation fails.
  async function perform(run, done, reportError) {
    if (!view || busy) return;
    const session = generation;
    ++readSequence;
    loading = false;
    lock(true);
    message('正在应用配置…');
    async function post(payload, revision = view.revision) {
      const result = await api(path, {method: 'POST', body: JSON.stringify({...payload, revision})});
      if (session !== generation) throw Error('会话已结束');
      render(result);
    }
    try {
      await run(post);
      if (session !== generation) return;
      const failed = view.nodes.some(n => n.error);
      message(failed ? '配置已保存。请在「节点与接入」查看错误并重试应用。' : '操作已完成。', failed ? 'warn' : 'good');
      done?.();
    } catch (error) {
      if (session !== generation) return;
      message(error.message, 'warn');
      reportError?.(error.message);
    } finally {
      if (session === generation) lock(false);
    }
  }

  function mutate(payload) { return perform(post => post(payload)); }

  function attach(e) {
    $('linkAttachMessage').textContent = '正在为 ' + e.name + ' 分配地址…';
    perform(post => post({action: 'attach', endpoint: ref(e)}), () => $('linkAttachDialog').close(),
      error => { $('linkAttachMessage').textContent = error; });
  }

  function save(event) {
    event.preventDefault();
    if ($('linkSave').disabled) return;
    const [a, b] = [catalog.get(pair.a), catalog.get(pair.b)];
    const rules = Array.from($('linkRules').children, row => {
      const field = n => row.querySelector('[name="' + n + '"]');
      const port = n => field(n).disabled ? 0 : Number(field(n).value);
      return {direction: field('direction').value, protocol: field('protocol').value, first: port('first'),
        last: port('last') || port('first'), enabled: field('enabled').checked};
    });
    const connection = {id: editing, a: ref(a), b: ref(b), enabled: $('linkEnabled').checked, rules};
    const revision = baseRevision;
    perform(async post => {
      for (const e of [a, b]) {
        if (!e.ip) await post({action: 'attach', endpoint: ref(e)});
      }
      await post({action: 'save', connection}, revision ?? view.revision);
    }, () => { startDraft(); setTab('connections'); $('linkCreate').focus({preventScroll: true}); });
  }

  async function refresh() {
    if (busy) return;
    const seq = ++readSequence;
    loading = true;
    $('linkRefresh').disabled = true;
    message('正在检查节点…');
    try {
      const v = await api(path);
      if (seq !== readSequence || platform.page !== 'interconnect') return;
      render(v);
      message();
    } catch (error) {
      if (seq === readSequence) message(error.message, 'warn');
    } finally {
      if (seq === readSequence) { loading = false; $('linkRefresh').disabled = busy; }
    }
  }

  function clearProbe() {
    ++probeSequence;
    $('linkProbeResult').textContent = '尚未测试';
    $('linkProbeForm').querySelector('button').disabled = false;
  }

  function openProbe(c) {
    probeID = c.id;
    const a = identity(catalog.get(key(c.a))), b = identity(catalog.get(key(c.b)));
    $('linkProbePair').replaceChildren(new Option(a + ' → ' + b, 'ab'), new Option(b + ' → ' + a, 'ba'));
    $('linkProbePort').value = '';
    clearProbe();
    $('linkProbeDialog').showModal();
    $('linkProbePort').focus();
  }

  async function probe(event) {
    event.preventDefault();
    const c = view.connections.find(c => c.id === probeID && c.enabled && !c.removing);
    if (!c) return;
    const ab = $('linkProbePair').value === 'ab', seq = ++probeSequence;
    const submit = $('linkProbeForm').querySelector('button');
    submit.disabled = true;
    $('linkProbeResult').textContent = '正在测试 TCP 连接…';
    try {
      await api(path, {method: 'POST', body: JSON.stringify({action: 'probe', probe: {
        from: ab ? c.a : c.b, to: ab ? c.b : c.a, port: Number($('linkProbePort').value)
      }})});
      if (seq === probeSequence) $('linkProbeResult').textContent = 'TCP 连接成功';
    } catch (error) {
      if (seq === probeSequence) $('linkProbeResult').textContent = 'TCP 连接失败 · ' + error.message;
    } finally {
      if (seq === probeSequence) submit.disabled = false;
    }
  }

  document.querySelectorAll('[data-link-tab]').forEach(button => { button.onclick = () => setTab(button.dataset.linkTab); });
  document.querySelectorAll('[data-ic-close]').forEach(button => { button.onclick = () => button.closest('dialog').close(); });
  $('linkEditor').onsubmit = save;
  $('linkAddRule').onclick = () => { rule(); $('linkRules').lastElementChild.querySelector('select').focus(); };
  $('linkCreate').onclick = $('linkNew').onclick = () => { startDraft(); setTab('build'); focusPicker(); };
  $('linkReload').onclick = () => { const c = view.connections.find(c => c.id === editing); if (c) startDraft(c); };
  $('linkPoolForm').onsubmit = event => { event.preventDefault(); mutate({action: 'settings', pool: $('linkPool').value.trim()}); };
  $('linkRetry').onclick = () => mutate({action: 'retry'});
  $('linkRefresh').onclick = refresh;
  $('linkAttach').onclick = () => {
    attachPicker.clear();
    $('linkAttachMessage').textContent = '';
    $('linkAttachDialog').showModal();
    attachPicker.search.focus();
  };
  $('linkProbeForm').onsubmit = probe;
  $('linkProbePair').onchange = $('linkProbePort').oninput = $('linkProbeDialog').onclose = clearProbe;

  function leave() {
    ++readSequence;
    loading = false;
    for (const id of ['linkAttachDialog', 'linkProbeDialog']) if ($(id).open) $(id).close();
  }

  function reset() {
    leave();
    ++generation;
    view = null;
    catalog.clear();
    nodes.clear();
    directory.clear();
    attachPicker.clear();
    directory.sync();
    attachPicker.sync();
    for (const id of ['linkNodes', 'linkEndpoints', 'linkConnections', 'linkVersion', 'linkProbePair']) $(id).replaceChildren();
    for (const id of ['linkNodeCount', 'linkEndpointCount', 'linkConnectionCount', 'linkHeroPool']) $(id).textContent = '—';
    $('linkPool').value = '';
    setTab('build');
    message();
    startDraft();
    lock(false);
  }

  startDraft();
  window.InterconnectUI = {open: refresh, leave, reset};
})();
