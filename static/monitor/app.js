(function(){
  const els = {
    statusText: document.getElementById('statusText'),
    activeClients: document.getElementById('activeClients'),
    totalClients: document.getElementById('totalClients'),
    activePublicConns: document.getElementById('activePublicConns'),
    totalPublicAccepted: document.getElementById('totalPublicAccepted'),
    portBindings: document.getElementById('portBindings'),
    tokensLoaded: document.getElementById('tokensLoaded'),
    memAlloc: document.getElementById('memAlloc'),
    goRoutines: document.getElementById('goRoutines'),
    recentErrors: document.getElementById('recentErrors'),
    uptime: document.getElementById('uptime'),
    serverAddr: document.getElementById('serverAddr'),
    httpAddr: document.getElementById('httpAddr'),
  };

  const logPanel = document.querySelector('.log-panel');
  // 事件显示状态：items为当前显示的事件（最新在前），不分页，最多保留 100 条
  const eventsState = { items: [] };
  // 全局提示与安全请求封装
  const toastEl = document.getElementById('toast');
  function showToast(msg, title){
    if(!toastEl) return;
    const t = toastEl.querySelector('.title');
    const m = toastEl.querySelector('.msg');
    if(t) t.textContent = title || '提示';
    if(m) m.textContent = msg || '请求失败，请稍后重试';
    toastEl.style.display = 'block';
    setTimeout(()=>{ toastEl.style.display='none'; }, 3000);
  }
  // 对话框提示
  const dialogBackdrop = document.getElementById('dialogBackdrop');
  const dialogTitle = document.getElementById('dialogTitle');
  const dialogMsg = document.getElementById('dialogMsg');
  const dialogClose = document.getElementById('dialogClose');
  function showDialog(title, msg){
    if(dialogTitle) dialogTitle.textContent = title || '提示';
    if(dialogMsg) dialogMsg.textContent = msg || '';
    if(dialogBackdrop){
      dialogBackdrop.style.display = 'flex';
      dialogBackdrop.classList.add('show');
    }
  }
  dialogClose && dialogClose.addEventListener('click', ()=>{
    if(dialogBackdrop){
      dialogBackdrop.style.display = 'none';
      dialogBackdrop.classList.remove('show');
    }
  });
  function setStatus(msg){
    if(!msg) return;
    showDialog('提示', msg);
  }
  async function safeFetch(url, options){
    try{
      const resp = await fetch(url, options);
      if(resp.status === 401){
        // 未登录，跳转登录页
        location.href = '/login';
        throw new Error('Unauthorized');
      }
      if(!resp.ok){
        const text = await resp.text().catch(()=> '');
        showToast(text || ('请求失败：' + resp.status));
        throw new Error('HTTP ' + resp.status);
      }
      return resp;
    }catch(e){
      if(!(e && e.message==='Unauthorized')){
        showToast('网络错误：' + e.message);
      }
      throw e;
    }
  }
  // 登录与授权管理
  const loginBox = document.getElementById('loginBox');
  const btnLogin = document.getElementById('btnLogin');
  const adminPass = document.getElementById('adminPass');
  const loginStatus = document.getElementById('loginStatus');

  const mgrBox = document.getElementById('mgrBox');
  const btnAdd = document.getElementById('btnAdd');
  const tok = document.getElementById('tok');
  const cid = document.getElementById('cid');
  const portsInput = document.getElementById('ports');

  const searchToken = document.getElementById('searchToken');
  const btnRefresh = document.getElementById('btnRefresh');
  const tokenTableBody = document.getElementById('tokenTableBody');
  const modalBackdrop = document.getElementById('modalBackdrop');
  const btnCancelDel = document.getElementById('btnCancelDel');
  const btnConfirmDel = document.getElementById('btnConfirmDel');
  let pendingDeleteToken = null;
  let editingToken = null;

  function parsePorts(s){
    if(!s) return [];
    return s.split(',').map(x=>parseInt(x.trim(),10)).filter(x=>!isNaN(x));
  }
  async function apiLogin(){
    loginStatus.textContent = '';
    try{
      const resp = await safeFetch('/login', {
        method: 'POST',
        headers: {'Content-Type':'application/json'},
        body: JSON.stringify({password: adminPass.value || ''}),
      });
      if(!resp.ok){ loginStatus.textContent = '登录失败'; return; }
      loginStatus.textContent = '登录成功';
      loginBox.style.display = 'none';
      mgrBox.style.display = 'block';
      await loadTokens();
    }catch(e){
      loginStatus.textContent = '网络错误';
    }
  }
  function renderTable(items){
    tokenTableBody.innerHTML = '';
    const q = (searchToken.value || '').toLowerCase();
    items
      .filter(it=>{
        const s = `${it.token} ${it.client_id || ''}`.toLowerCase();
        return !q || s.includes(q);
      })
      .forEach(it=>{
        const tr = document.createElement('tr');

        const tdTok = document.createElement('td');
        tdTok.textContent = it.token;
        tr.appendChild(tdTok);

        const tdCid = document.createElement('td');
        tdCid.textContent = it.client_id || '(不限)';
        tr.appendChild(tdCid);

        const tdPorts = document.createElement('td');
        const chips = document.createElement('div');
        chips.className = 'chips';
        const ports = (it.ports && it.ports.length) ? it.ports : ['(不限)'];
        ports.forEach(p=>{
          const chip = document.createElement('span');
          chip.className = 'chip';
          chip.textContent = p;
          chips.appendChild(chip);
        });
        tdPorts.appendChild(chips);
        tr.appendChild(tdPorts);

        const tdAct = document.createElement('td');
        tdAct.className = 'actions';

        const btnEdit = document.createElement('button');
        btnEdit.className = 'btn secondary';
        btnEdit.textContent = '修改';
        btnEdit.onclick = ()=>{
          // 进入编辑态：填充表单，禁用 token 字段
          editingToken = it.token;
          tok.value = it.token;
          tok.setAttribute('disabled', 'disabled');
          cid.value = it.client_id || '';
          portsInput.value = (it.ports && it.ports.length) ? it.ports.join(',') : '';
          btnAdd.textContent = '保存修改';
          setStatus('');
        };
        tdAct.appendChild(btnEdit);

        const btnDel = document.createElement('button');
        btnDel.className = 'btn danger';
        btnDel.style.marginLeft = '8px';
        btnDel.textContent = '删除';
        btnDel.onclick = ()=>{
          pendingDeleteToken = it.token;
          document.getElementById('modalMsg').textContent = `确定删除授权: ${it.token} ?`;
          modalBackdrop.classList.add('show');
        };
        tdAct.appendChild(btnDel);

        tr.appendChild(tdAct);

        tokenTableBody.appendChild(tr);
      });
  }

  async function loadTokens(){
    setStatus('');
    try{
      const resp = await safeFetch('/api/tokens');
      if(!resp.ok){ setStatus('加载失败'); return; }
      const data = await resp.json();
      renderTable(data.items || []);
    }catch(e){
      setStatus('网络错误');

    }
  }
  function resetEdit(){
    editingToken = null;
    tok.removeAttribute('disabled');
    tok.value = '';
    cid.value = '';
    portsInput.value = '';
    btnAdd.textContent = '新增授权';
  }

  async function addToken(){
    // 避免重复提示：操作前清空提示并禁用按钮
    setStatus('');
    btnAdd.setAttribute('disabled', 'disabled');
    try{
      const payload = {
        token: editingToken ? editingToken : tok.value,
        client_id: cid.value,
        ports: parsePorts(portsInput.value),
      };
      if(!payload.token || payload.token.trim() === ''){
        setStatus('Token 不能为空');
        return;
      }
      const resp = await safeFetch('/api/tokens', {
        method: 'POST',
        headers: {'Content-Type':'application/json'},
        body: JSON.stringify(payload),
      });
      if(!resp.ok){
        setStatus('保存失败');
        return;
      }
      setStatus(editingToken ? '修改已保存' : '保存成功');
      await loadTokens();
      resetEdit();
    }catch(e){
      setStatus('网络错误');
      showDialog('网络错误', (e && e.message) ? e.message : '请稍后重试');
    }finally{
      btnAdd.removeAttribute('disabled');
    }
  }
  // 登录在登录页处理，这里无需按钮
  btnAdd && btnAdd.addEventListener('click', addToken);
  btnRefresh && btnRefresh.addEventListener('click', ()=>{
    setStatus('');
    loadTokens();
  });
  searchToken && searchToken.addEventListener('input', ()=>{
    setStatus('');
    loadTokens();
  });
  // 页面加载后默认拉取授权列表
  loadTokens();
  btnCancelDel && btnCancelDel.addEventListener('click', ()=>{ modalBackdrop.classList.remove('show'); pendingDeleteToken=null;});
  btnConfirmDel && btnConfirmDel.addEventListener('click', async ()=>{
    if(!pendingDeleteToken) return;
    try{
      const r = await safeFetch('/api/tokens/'+encodeURIComponent(pendingDeleteToken), {method:'DELETE'});
      if(r.ok){ setStatus('删除成功'); await loadTokens(); }
      else{ setStatus('删除失败'); }
    }catch(e){ setStatus('网络错误'); }
    modalBackdrop.classList.remove('show'); pendingDeleteToken=null;
  });

  function humanBytes(n){
    if(n < 1024) return n + ' B';
    const units = ['KB','MB','GB','TB'];
    let i=0, v=n/1024;
    while(v>=1024 && i<units.length-1){ v/=1024; i++; }
    return v.toFixed(1)+' '+units[i];
  }
  function humanDuration(sec){
    const d = Math.floor(sec/86400); sec%=86400;
    const h = Math.floor(sec/3600); sec%=3600;
    const m = Math.floor(sec/60); sec%=60;
    const s = Math.floor(sec);
    const parts=[];
    if(d) parts.push(d+'天');
    if(h) parts.push(h+'小时');
    if(m) parts.push(m+'分');
    parts.push(s+'秒');
    return parts.join(' ');
  }

  function renderRecent(items){
    els.recentErrors.innerHTML = '';
    items.forEach(item=>{
      const li = document.createElement('li');
      if(item.level === 'error') li.classList.add('err');
      const t = document.createElement('span');
      t.className = 'time';
      t.textContent = (item.time || '') + (item.client_id ? ` [${item.client_id}]` : '');
      const msg = document.createElement('span');
      msg.textContent = item.message || '';
      li.appendChild(t);
      li.appendChild(msg);
      els.recentErrors.appendChild(li);
    });
  }




  let es;
  function connect(){
    if(es) es.close();
    els.statusText.textContent = '连接中...';
    es = new EventSource('/events');
    es.onopen = () => { els.statusText.textContent = '已连接'; };
    es.onerror = () => { els.statusText.textContent = '连接断开或未登录，跳转登录页...'; try{ es && es.close(); }catch{} location.href = '/login'; };
    es.onmessage = (e) => {
      try {
        const data = JSON.parse(e.data);
        els.activeClients.textContent = data.active_clients;
        els.totalClients.textContent = '累计 ' + data.total_clients;
        els.activePublicConns.textContent = data.active_public_conns;
        els.totalPublicAccepted.textContent = '累计 ' + data.total_public_accepted;
        els.portBindings.textContent = data.port_bindings;
        els.tokensLoaded.textContent = '授权令牌 ' + data.tokens_loaded;

        els.memAlloc.textContent = humanBytes(data.mem_alloc || 0);
        els.goRoutines.textContent = '协程 ' + (data.goroutines || 0);
        els.uptime.textContent = '运行时长 ' + humanDuration(data.uptime_seconds || 0);

        els.serverAddr.textContent = data.server_addr || '-';
        els.httpAddr.textContent = data.http_addr || '-';

        const list = data.recent_events || [];
        eventsState.items = list.slice().reverse();
        if(eventsState.items.length > 100){
          eventsState.items = eventsState.items.slice(0, 100);
        }
        renderRecent(eventsState.items);
      } catch(err){
        console.error('parse sse', err);
      }
    };
  }
  connect();
  window.addEventListener('visibilitychange', ()=>{
    if(document.visibilityState === 'visible') connect();
  });




})();