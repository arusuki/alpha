'use strict';
(()=>{
const state={schema:null,busy:false,epoch:0,deleting:null,resetting:null};
const admin=()=>platform.user?.role==='admin';
function controls(){
  $('memberSchemaEditor').disabled=state.busy||!state.schema;
  for(const id of ['membersRefresh','memberReloadSchema','memberCreateInvitation','memberDeleteSubmit','memberDeleteCancel','memberDeleteConfirm','memberPasswordSubmit','memberPasswordCancel','memberPassword','memberPasswordConfirm'])$(id).disabled=state.busy;
  document.querySelectorAll('[data-reset-member],[data-delete-member],[data-revoke-invitation] button,[data-edit-invitation],[data-delete-invitation] button').forEach(el=>el.disabled=state.busy);
}
function fields(){
  return [...$('memberSchemaFields').querySelectorAll('[data-member-field]')].map(row=>{
    const read=key=>row.querySelector(`[data-field-${key}]`);
    const f={key:read('key').value.trim(),label:read('label').value.trim(),type:read('type').value,required:read('required').checked};
    if(f.type==='select')f.options=read('options').value.split('\n').map(x=>x.trim()).filter(Boolean);
    return f;
  });
}
function preview(value){
  $('memberSchemaPreview').innerHTML='<label>使用者标识 *<input disabled placeholder="例如 alice"></label><label>SSH 公钥 *<input disabled placeholder="ssh-ed25519 …"></label>'+value.map(f=>`<label>${esc(f.label||'未命名字段')}${f.required?' *':''}${f.type==='select'?`<select disabled><option>请选择</option>${(f.options||[]).map(o=>`<option>${esc(o)}</option>`).join('')}</select>`:'<input disabled placeholder="自行填写">'}</label>`).join('');
}
function renderFields(value){
  $('memberSchemaFields').innerHTML=value.map((f,i)=>`<div class="member-schema-field" data-member-field="${i}"><div class="member-field-heading"><strong>字段 ${i+1}</strong><button type="button" data-remove-field="${i}">移除</button></div><div class="form-grid three"><label>字段标识<input data-field-key value="${esc(f.key)}" required maxlength="48" pattern="[a-z][a-z0-9_]*" placeholder="例如 full_name"></label><label>显示名称<input data-field-label value="${esc(f.label)}" required maxlength="80" placeholder="例如 姓名"></label><label>填写方式<select data-field-type><option value="text" ${f.type==='text'?'selected':''}>文本填写</option><option value="select" ${f.type==='select'?'selected':''}>单选</option></select></label></div><label class="checkbox-label"><input data-field-required type="checkbox" ${f.required?'checked':''}>必填</label><label class="member-field-options" ${f.type==='select'?'':'hidden'}>可选项（每行一项）<textarea data-field-options rows="3" ${f.type==='select'?'required':'disabled'}>${esc((f.options||[]).join('\n'))}</textarea></label></div>`).join('')||'<p class="form-note">尚未设置附加字段。注册时需使用者标识、邀请码和 SSH 公钥。</p>';
  $('memberAddField').disabled=value.length>=32;
  preview(value);
}
function renderSchema(schema){
  state.schema=schema;renderFields(schema.fields);
  $('memberSchemaStatus').textContent=`已保存 · 版本 ${schema.revision}`;
}
function changed(){ $('memberSchemaStatus').textContent='有未保存的修改';preview(fields()); }
function renderMembers(members){
  $('membersBody').innerHTML=members.map(m=>`<tr><td><strong>${esc(m.username)}</strong><small class="sub mono">${esc(m.id)}</small><a class="sub" href="/status/${encodeURIComponent(m.username)}" target="_blank" rel="noopener">使用者状态页 ↗</a></td><td>${m.schema.fields.filter(f=>Object.hasOwn(m.profile,f.key)).map(f=>`<span class="sub">${esc(f.label)}：${esc(m.profile[f.key])}</span>`).join('')||'—'}</td><td>${esc(dateTime(m.created_at))}</td><td><button data-reset-member="${esc(m.id)}" data-username="${esc(m.username)}">重置密码</button> <button class="danger" data-delete-member="${esc(m.id)}" data-username="${esc(m.username)}">删除</button></td></tr>`).join('')||'<tr><td colspan="4" class="empty">暂无使用者。配置注册信息并发放邀请码后，可通过注册 API 登记。</td></tr>';

}
async function load(epoch,reloadSchema=false){
  const [members,invitations,schema]=await Promise.all([api('/api/members'),invitationPage(),reloadSchema||!state.schema?api('/api/members/registration-schema'):null]);
  if(epoch!==state.epoch)return;
  renderMembers(members.members);$('memberInvitationsPanel').innerHTML=invitations;
  if(schema)renderSchema(schema);
}
async function task(fn){
  if(state.busy||!admin())return;
  const epoch=state.epoch;state.busy=true;controls();$('membersError').textContent='';
  try{await fn(epoch);}catch(error){if(epoch===state.epoch)$(state.resetting?'memberPasswordError':state.deleting?'memberDeleteError':'membersError').textContent=error.message;}
  finally{if(epoch===state.epoch){state.busy=false;controls();}}
}
function clearCode(){ $('memberInvitationCode').value='';$('memberInvitationCopyStatus').textContent=''; }
window.MembersUI={
  open(){return task(epoch=>load(epoch));},
  confirmDelete(id,username,onDeleted=null){
    if(state.busy||!admin())return;
    state.deleting={id,username,onDeleted};$('memberDeleteForm').reset();$('memberDeleteError').textContent='';
    $('memberDeleteTitle').textContent='删除使用者 · '+username;$('memberDeleteDialog').showModal();$('memberDeleteConfirm').focus();
  },
  resetPassword(id,username){
    if(state.busy||!admin())return;
    state.resetting={id,username};$('memberPasswordForm').reset();
    $('memberPasswordError').textContent='';$('memberPasswordStatus').textContent='';
    $('memberPasswordSubmit').hidden=false;$('memberPasswordFields').hidden=false;
    $('memberPasswordCancel').textContent='取消';
    $('memberPasswordTitle').textContent='重置密码 · '+username;
    $('memberPasswordDialog').showModal();$('memberPassword').focus();
  },
  invitationOptions(){return invitationPage(null,'/admin/member-invitations/select');},
  reset(){
    state.epoch++;state.schema=null;state.busy=false;state.deleting=null;state.resetting=null;$('memberDeleteDialog').close();$('memberPasswordDialog').close();$('memberPasswordForm').reset();
    if($('memberInvitationDialog').open)$('memberInvitationDialog').close();clearCode();
    for(const id of ['membersBody','memberInvitationsPanel','memberSchemaFields','memberSchemaPreview'])$(id).innerHTML='';
    for(const id of ['membersError','membersStatus','memberSchemaStatus'])$(id).textContent='';
    $('memberInvitationForm').reset();controls();
  }
};
$('membersBody').addEventListener('click',event=>{
  const reset=event.target.closest('[data-reset-member]');if(reset){window.MembersUI.resetPassword(reset.dataset.resetMember,reset.dataset.username);return;}
  const button=event.target.closest('[data-delete-member]');if(button)window.MembersUI.confirmDelete(button.dataset.deleteMember,button.dataset.username);
});
$('memberPasswordCancel').addEventListener('click',()=>$('memberPasswordDialog').close());
$('memberPasswordDialog').addEventListener('cancel',event=>{if(state.busy)event.preventDefault();});
$('memberPasswordDialog').addEventListener('close',()=>{state.resetting=null;$('memberPasswordForm').reset();});
$('memberPasswordForm').addEventListener('submit',event=>{event.preventDefault();task(async epoch=>{
  const target=state.resetting;if(!target)return;
  $('memberPasswordError').textContent='';
  const password=$('memberPassword').value;
  if(password!==$('memberPasswordConfirm').value)throw Error('两次输入的密码不一致。');
  const length=new TextEncoder().encode(password).length;
  if(length<12||length>256||/[\x00\r\n:]/.test(password))throw Error('密码需为 12–256 字节，不能含换行、冒号或 NUL。');
  $('memberPasswordStatus').textContent='正在重置账号与容器密码…';
  let result;
  try{result=await api(`/api/members/${target.id}/password`,{method:'POST',body:JSON.stringify({password})});}
  catch(error){if(epoch===state.epoch)$('memberPasswordStatus').textContent='操作未确认完成；如请求中断，请使用相同密码重试。';throw error;}
  if(epoch!==state.epoch)return;
  if(!result.account_reset)throw Error('服务未确认账号密码已重置，请重试。');
  if(!result.ok){
    $('memberPasswordStatus').textContent=`账号密码已重置，旧登录会话已注销，已更新 ${result.updated} 个容器。请修复下列问题后使用相同密码重试。`;
    $('memberPasswordError').textContent=(result.nodes||[]).filter(n=>!n.ok).map(n=>`${n.node_name}：${(n.errors||[]).join('；')}`).join('；');
    return;
  }
  $('memberPasswordForm').reset();$('memberPasswordFields').hidden=true;$('memberPasswordSubmit').hidden=true;
  $('memberPasswordCancel').textContent='关闭';
  $('memberPasswordStatus').textContent=`密码重置完成：账号及 ${result.updated} 个容器已更新，旧登录会话已注销。`;
});});
$('memberDeleteCancel').addEventListener('click',()=>$('memberDeleteDialog').close());
$('memberDeleteDialog').addEventListener('cancel',event=>{if(state.busy)event.preventDefault();});
$('memberDeleteDialog').addEventListener('close',()=>{state.deleting=null;$('memberDeleteConfirm').value='';});
$('memberDeleteForm').addEventListener('submit',event=>{event.preventDefault();task(async epoch=>{
  const target=state.deleting;if(!target)return;
  if($('memberDeleteConfirm').value!==target.username)throw Error('请填写完整使用者标识确认删除。');
  await api(`/api/members/${target.id}`,{method:'DELETE',body:'{}'});if(epoch!==state.epoch)return;
  $('memberDeleteDialog').close();$('membersStatus').textContent='使用者已删除，容器保留并标为未归属，等待管理员手动回收。';
  if(target.onDeleted)await target.onDeleted();else await load(epoch);
});});
$('membersRefresh').addEventListener('click',()=>task(epoch=>load(epoch)));
$('memberReloadSchema').addEventListener('click',()=>task(epoch=>load(epoch,true)));
$('memberAddField').addEventListener('click',()=>{
  const next=fields();if(next.length>=32)return;next.push({key:'',label:'',type:'text',required:true});renderFields(next);changed();
  $('memberSchemaFields').lastElementChild.querySelector('[data-field-key]').focus();
});
$('memberExampleFields').addEventListener('click',()=>{
  const next=fields(),keys=new Set(next.map(f=>f.key));
  const examples=[{key:'full_name',label:'姓名',type:'text',required:true},{key:'degree',label:'学历',type:'select',required:true,options:['博士','硕士']},{key:'group',label:'组别',type:'select',required:true,options:['A组','B组']}];
  for(const f of examples)if(!keys.has(f.key)&&next.length<32)next.push(f);
  renderFields(next);changed();
});
$('memberSchemaFields').addEventListener('click',event=>{
  const button=event.target.closest('[data-remove-field]');if(!button)return;
  const next=fields();next.splice(Number(button.dataset.removeField),1);renderFields(next);changed();
});
$('memberSchemaFields').addEventListener('input',changed);
$('memberSchemaFields').addEventListener('change',event=>{
  if(event.target.matches('[data-field-type]')){
    const row=event.target.closest('[data-member-field]'),select=event.target.value==='select';
    row.querySelector('.member-field-options').hidden=!select;
    row.querySelector('[data-field-options]').disabled=!select;
    row.querySelector('[data-field-options]').required=select;
  }
  changed();
});
$('memberSchemaForm').addEventListener('submit',event=>{event.preventDefault();task(async epoch=>{
  if(!state.schema)throw Error('请先载入注册配置');
  const result=await api('/api/members/registration-schema',{method:'PUT',body:JSON.stringify({revision:state.schema.revision,fields:fields()})});
  if(epoch!==state.epoch)return;
  renderSchema(result);$('membersStatus').textContent='注册信息配置已保存。';
});});
// Invitation operations submit admin forms and receive a server-rendered view.
async function invitationPage(form=null,path='/admin/member-invitations'){
  const response=await fetch(form?form.action:path,{credentials:'same-origin',cache:'no-store',method:form?'POST':'GET',headers:{'X-CSRF-Token':platform.csrf},...(form?{body:new URLSearchParams(new FormData(form))}:{})});
  if(!response.ok){
    const error=await response.json();
    if(response.status===401&&platform.user)showAuth(false,'会话已过期，请重新登录。');
    throw Error(error.error||'邀请码操作失败');
  }
  return response.text();
}
function submitInvitation(event){
  const form=event.target;event.preventDefault();
  task(async epoch=>{
    let html;
    try{html=await invitationPage(form);}
    catch(error){
      if(epoch===state.epoch&&form.matches('[data-update-invitation]')&&form.isConnected)editInvitationLabel(form);
      throw error;
    }
    if(epoch!==state.epoch)return;
    $('memberInvitationsPanel').innerHTML=html;
    const issued=$('memberInvitationsPanel').querySelector('[data-issued-invitation]');
    if(issued){
      clearCode();$('memberInvitationCode').value=issued.value;issued.remove();$('memberInvitationDialog').showModal();
      $('membersStatus').textContent='邀请码已生成，请复制保存并发给使用者。';
    }else if(form.matches('[data-update-invitation]'))$('membersStatus').textContent='邀请码备注已保存。';
    else if(form.matches('[data-delete-invitation]'))$('membersStatus').textContent='邀请码已删除，已登记的使用者不受影响。';
    else $('membersStatus').textContent='邀请码已作废，已登记的使用者不受影响。';
  });
}
function editInvitationLabel(form){
  form.parentElement.querySelector('[data-edit-invitation]').hidden=true;form.hidden=false;
  form.elements.label.focus();form.elements.label.select();
}
function finishInvitationLabel(form,save=true,refocus=false){
  if(form.hidden)return;
  const input=form.elements.label,button=form.parentElement.querySelector('[data-edit-invitation]');
  form.hidden=true;button.hidden=false;
  if(refocus)button.focus();
  if(save&&input.value.trim()!==input.defaultValue)form.requestSubmit();
  else input.value=input.defaultValue;
}
$('memberInvitationsPanel').addEventListener('click',event=>{
  const button=event.target.closest('[data-edit-invitation]');
  if(button&&!state.busy&&admin())editInvitationLabel(button.parentElement.querySelector('[data-update-invitation]'));
});
$('memberInvitationsPanel').addEventListener('focusout',event=>{
  if(event.target.matches('[data-update-invitation] input[name=label]'))finishInvitationLabel(event.target.form);
});
$('memberInvitationsPanel').addEventListener('keydown',event=>{
  if(!event.target.matches('[data-update-invitation] input[name=label]')||event.isComposing)return;
  if(event.key==='Enter'||event.key==='Escape'){
    event.preventDefault();finishInvitationLabel(event.target.form,event.key==='Enter',true);
  }
});
$('memberInvitationForm').addEventListener('submit',submitInvitation);
$('memberInvitationsPanel').addEventListener('submit',submitInvitation);
$('memberInvitationCopy').addEventListener('click',async()=>{
  const code=$('memberInvitationCode').value,epoch=state.epoch;
  try{await AlphaClipboard.writeText(code);if(epoch===state.epoch&&code===$('memberInvitationCode').value)$('memberInvitationCopyStatus').textContent='已复制。';}
  catch(_){if(epoch===state.epoch&&code===$('memberInvitationCode').value){$('memberInvitationCode').select();$('memberInvitationCopyStatus').textContent='请手动复制所选邀请码。';}}
});
$('memberInvitationClose').addEventListener('click',()=>{clearCode();$('memberInvitationDialog').close();});
$('memberInvitationDialog').addEventListener('close',clearCode);
})();
