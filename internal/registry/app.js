(() => {
  'use strict';
  const $ = id => document.getElementById(id);
  const base = document.body.dataset.base;
  let csrf = '', schema, stream, nodeChoices = null, optionsRequest = 0, optionsLoading = false;
  const message = text => { $('message').textContent = text; };
  async function api(action, body) {
    const response = await fetch(`${base}/api/${action}`, {
      method: body === undefined ? 'GET' : 'POST', credentials: 'same-origin', cache: 'no-store',
      headers: body === undefined ? {} : { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf },
      body: body === undefined ? undefined : JSON.stringify(body)
    });
    const result = await response.json();
    if (!response.ok) { const error = new Error(result.error || '请求失败'); error.status = response.status; throw error; }
    return result;
  }
  function showFields() {
    $('fields').replaceChildren();
    for (const field of schema.fields) {
      const label = document.createElement('label');
      label.append(document.createTextNode(field.label + (field.required ? '' : '（选填）')));
      const input = document.createElement(field.type === 'select' ? 'select' : 'input');
      input.dataset.key = field.key;
      input.required = field.required;
      if (field.type === 'select') {
        for (const value of ['', ...field.options]) {
          const option = document.createElement('option'); option.value = value; option.textContent = value || '请选择'; input.append(option);
        }
      } else { input.maxLength = 512; }
      label.append(input); $('fields').append(label);
    }
  }
  function choiceOption(node, value, title, detail, claimed = false) {
    const label = document.createElement('label'); label.className = 'container-option';
    const input = document.createElement('input');
    input.type = 'radio'; input.name = 'container-' + node.node_id; input.dataset.node = node.node_id;
    input.value = value; input.required = true; input.disabled = claimed;
    const content = document.createElement('span'); content.className = 'option-content';
    const heading = document.createElement('span'); heading.className = 'option-title'; heading.textContent = title;
    const description = document.createElement('span'); description.className = 'option-detail'; description.textContent = detail;
    content.append(heading, description); label.append(input, content);
    if (claimed) { const badge = document.createElement('span'); badge.className = 'claimed-badge'; badge.textContent = '已领养'; label.append(badge); }
    return label;
  }
  function updateChoiceSummary() {
    const selected = $('nodeChoices').querySelectorAll('input:checked').length;
    $('choiceSummary').textContent = nodeChoices?.length ? `已选择 ${selected} / ${nodeChoices.length} 个节点` : '';
    for (const card of $('nodeChoices').querySelectorAll('.choice-node')) {
      const input = card.querySelector('input:checked');
      card.querySelector('.choice-state').textContent = input ? input.value === 'create' ? '将新建' : '将领养' : '待选择';
      card.classList.toggle('has-choice', !!input);
    }
  }
  $('nodeChoices').addEventListener('change', updateChoiceSummary);
  async function loadNodes() {
    const request = ++optionsRequest;
    optionsLoading = true; $('submit').disabled = true; $('reloadNodes').disabled = true;
    const previous = new Map(Array.from($('nodeChoices').querySelectorAll('input:checked')).map(el => [el.dataset.node, el.value]));
    try {
      const value = await api('options', {username:$('registration').elements.username.value});
      if (request !== optionsRequest) return;
      nodeChoices = value.nodes;
      $('nodeChoices').replaceChildren();
      for (const [index, node] of nodeChoices.entries()) {
        const card = document.createElement('fieldset'); card.className = 'choice-node';
        const legend = document.createElement('legend'); legend.textContent = node.node_name;
        const heading = document.createElement('div'); heading.className = 'choice-node-heading';
        const number = document.createElement('span'); number.className = 'choice-index'; number.textContent = `NODE / ${String(index + 1).padStart(2, '0')}`;
        const status = document.createElement('span'); status.className = 'choice-state';
        heading.append(number, status); card.append(legend, heading);
        card.append(choiceOption(node, 'create', '新建一个', '使用默认环境，创建专属容器'));
        const caption = document.createElement('p'); caption.className = 'candidate-heading';
        caption.textContent = `领养已有容器 · ${node.containers.filter(c => !c.claimed).length} 个可选`; card.append(caption);
        const list = document.createElement('div'); list.className = 'candidate-list';
        for (const c of node.containers) list.append(choiceOption(node, c.id, c.name, `${c.id.slice(0,12)}${(c.claimed_by||c.owner)?' · '+(c.claimed_by||c.owner):''}`, c.claimed));
        if (!node.containers.length) { const empty = document.createElement('p'); empty.className = 'choice-empty'; empty.textContent = node.error ? '暂时无法读取已有容器' : '暂无已有容器，可以选择新建'; list.append(empty); }
        card.append(list);
        const old = previous.get(node.node_id);
        for (const input of card.querySelectorAll('input')) input.checked = input.value === old && !input.disabled;
        if (node.error) {const hint=document.createElement('p'); hint.className='choice-error'; hint.textContent='暂不能查询已有容器：'+node.error+'。可选择新建并在节点恢复后重试。'; card.append(hint);}
        $('nodeChoices').append(card);
      }
      if (!nodeChoices.length) $('nodeChoices').textContent='暂无计算节点；管理员添加节点后，可在状态页申请。';
      updateChoiceSummary();
    } catch (error) { if(request===optionsRequest){nodeChoices=null; message(error.message);} }
    finally {if(request===optionsRequest){optionsLoading=false; $('submit').disabled=nodeChoices===null; $('reloadNodes').disabled=false;}}
  }
  $('reloadNodes').onclick = loadNodes;
  $('registration').elements.username.addEventListener('change',loadNodes);
  const labels = { pending:'等待分配', running:'正在分配', ready:'已就绪', invited:'链接已生成', accepted:'已接受', creating:'正在生成链接', unknown:'需管理员核对', failed:'分配失败', unallocated:'尚未分配', deleting:'正在回收', deleted:'已回收' };
  function render(value) {
    const access = value.access || {};
    const rows = [ {name:'注册信息', state:'ready'},
      {name:'Tailscale 网络分享', state:access.invite_state, error:access.error},
      {name:'SSH 公钥与跳板访问', state:access.key_state, error:access.error},
      ...(value.nodes || []).filter(node => node.state !== 'unallocated').map(node => ({name:`${node.node_name} · 容器`, state:node.state, error:node.error})) ];
    $('steps').replaceChildren();
    let done = 0, failed = false;
    for (const row of rows) {
      const ready = ['ready','invited','accepted'].includes(row.state);
      const problem = !ready && (!!row.error || ['failed','unknown'].includes(row.state));
      done += Number(ready); failed ||= problem;
      const li = document.createElement('li'); li.className = ready ? 'ready' : problem ? 'failed' : 'pending';
      const title = document.createElement('strong'); title.textContent = row.name;
      const state = document.createElement('span'); state.textContent = labels[row.state] || '等待分配';
      li.append(title, state);
      if (problem && row.error) { const detail = document.createElement('p'); detail.textContent = row.error; li.append(detail); }
      $('steps').append(li);
    }
    const percent = Math.round(done / rows.length * 100);
    $('bar').value = percent; $('percent').textContent = `${percent}%`;
    $('summary').textContent = `${done} / ${rows.length} 项已完成`;
    $('progressTitle').textContent = percent === 100 ? '你的工作空间已就绪' : failed ? '部分资源需要处理' : '正在准备你的工作空间';
    $('retry').hidden = !failed;
    message(failed ? '已完成的资源会保留。可重试未完成项；分享结果未知时需管理员核对。' : percent === 100 ? '注册与资源分配已完成。' : '注册已完成，资源分配进度会自动更新。');
    let link;
    try { link = new URL(access.invite_url); } catch (_) { /* No link yet. */ }
    $('share').hidden = !(link && link.protocol === 'https:' && link.host === 'login.tailscale.com' && !link.username && !link.password && ['invited','accepted'].includes(access.invite_state));
    if (!$('share').hidden) { $('shareLink').href = link.href; $('shareLink').textContent = link.href; }
    let statusURL;
    try { statusURL = new URL(value.control.status_url); } catch (_) { /* Not available. */ }
    const guideReady = access.key_state === 'ready' && ['invited','accepted'].includes(access.invite_state)
      && statusURL && ['http:','https:'].includes(statusURL.protocol) && !statusURL.username && !statusURL.password;
    $('controlGuide').hidden = !guideReady;
    if (guideReady) {
      $('controlStatusLink').href = statusURL.href;
      $('controlAddress').textContent = statusURL.href;
    }
  }
  function watch() {
    $('registration').hidden = true; $('resume').hidden = true; $('progress').hidden = false;
    if (stream) stream.close();
    stream = new EventSource(`${base}/api/events`);
    stream.addEventListener('progress', event => { render(JSON.parse(event.data)); });
    stream.addEventListener('problem', event => { message(JSON.parse(event.data).error); });
    stream.onerror = () => message('进度连接中断，正在自动重连；已提交的注册会继续处理。');
  }
  async function register(body) {
    $('submit').disabled = true; $('resume').hidden = true;
    message('正在提交注册…');
    try { await api('register', body); $('registration').elements.password.value = ''; $('registration').elements.password_confirm.value = ''; watch(); }
    catch (error) {
      message(error.message);
      if (error.status >= 400 && error.status < 500) {
        $('registration').hidden = false;
        if (error.status === 409) message(`${error.message}。若注册字段已更新，请刷新页面。`);
      } else { $('registration').hidden = true; $('resume').hidden = false; }
    } finally { $('submit').disabled = optionsLoading || nodeChoices === null; }
  }
  $('registration').addEventListener('submit', event => {
    event.preventDefault();
    const form = $('registration');
    if (form.elements.password.value !== form.elements.password_confirm.value) { message('两次输入的密码不一致。'); return; }
    if (optionsLoading || nodeChoices === null) { message('请先载入节点与容器列表。'); return; }
    const containers = Array.from($('nodeChoices').querySelectorAll('input:checked')).map(input => ({node_id:input.dataset.node,mode:input.value==='create'?'create':'adopt',container_id:input.value==='create'?'':input.value}));
    if (containers.length !== nodeChoices.length) {message('请为每个节点选择领养或新建。');return;}
    const profile = Object.create(null);
    for (const input of $('fields').querySelectorAll('[data-key]')) profile[input.dataset.key] = input.value;
    register({password:$('registration').elements.password.value, username:$('registration').elements.username.value, ssh_public_key:$('registration').elements.ssh_public_key.value, schema_revision:schema.revision, profile, containers});
  });
  $('resume').onclick = () => register({});
  $('retry').onclick = async () => {
    $('retry').disabled = true;
    try { await api('retry', {}); message('已请求重试，正在等待分配结果。'); } catch (error) { message(error.message); }
    finally { $('retry').disabled = false; }
  };
  $('copy').onclick = async () => {
    try { await navigator.clipboard.writeText($('shareLink').href); message('分享链接已复制。'); }
    catch (_) { message('请长按或右键上方分享链接复制。'); }
  };
  addEventListener('pagehide', () => stream?.close());
  async function load() {
    try {
      const value = await api('session'); csrf = value.csrf; schema = value.schema;
      showFields();
      if (value.registered) watch();
      else if (value.submitted) await register({});
      else { $('registration').hidden = false; message('邀请码已验证，请填写注册信息并选择各节点的容器。'); await loadNodes(); }
    } catch (error) { message(`${error.message}，请刷新页面重试。`); }
  }
  load();
})();
