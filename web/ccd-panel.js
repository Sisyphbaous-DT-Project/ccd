// ccd 管理面板：注入 new-api 页面后，在日志页给管理员（role>=100）加一个「思维链转述」按钮。
// 点开可以配置「哪个分组 / 模型 / key 的思维链转述成什么语言、用哪个 key 的哪个模型转述」。
// 主题变量 --ccd-* 随 new-api 的 html.dark 实时切换。
(function () {
  var BTN = 'ccd-btn', MODAL = 'ccd-modal';

  var themeStyle = document.createElement('style');
  themeStyle.id = 'ccd-theme';
  var scope = '#' + MODAL + ',#' + BTN;
  themeStyle.textContent = scope + '{' +
    '--ccd-surface:#ffffff;--ccd-text:#18181b;--ccd-secondary:#52525b;--ccd-muted:#61616b;' +
    '--ccd-card:#f8fafc;--ccd-subtle:#f4f4f5;--ccd-track:#e4e4e7;--ccd-border:#d4d4d8;--ccd-hover:#e4e4e7;' +
    '--ccd-blue:#1d4ed8;--ccd-green:#15803d;--ccd-red:#b91c1c;--ccd-amber:#a16207;' +
    '--ccd-green-text:#166534;--ccd-red-text:#991b1b;--ccd-amber-text:#92400e;' +
    '--ccd-green-bg:#dcfce7;--ccd-red-bg:#fee2e2;--ccd-blue-bg:#dbeafe;--ccd-amber-bg:#fef3c7;' +
    '--ccd-overlay:rgba(0,0,0,.32);--ccd-shadow:0 12px 40px rgba(0,0,0,.18);color-scheme:light;}' +
    'html.dark ' + scope + '{' +
    '--ccd-surface:#18181b;--ccd-text:#e4e4e7;--ccd-secondary:#a1a1aa;--ccd-muted:#a1a1aa;' +
    '--ccd-card:rgba(255,255,255,.02);--ccd-subtle:rgba(255,255,255,.06);--ccd-track:rgba(255,255,255,.08);' +
    '--ccd-border:rgba(255,255,255,.1);--ccd-hover:rgba(255,255,255,.12);' +
    '--ccd-blue:#93c5fd;--ccd-green:#22c55e;--ccd-red:#f87171;--ccd-amber:#fbbf24;' +
    '--ccd-green-text:#86efac;--ccd-red-text:#fca5a5;--ccd-amber-text:#fcd34d;' +
    '--ccd-green-bg:rgba(34,197,94,.15);--ccd-red-bg:rgba(239,68,68,.15);--ccd-blue-bg:rgba(96,165,250,.15);--ccd-amber-bg:rgba(251,191,36,.15);' +
    '--ccd-overlay:rgba(0,0,0,.6);--ccd-shadow:0 12px 40px rgba(0,0,0,.45);color-scheme:dark;}' +
    '#ccd-modal input[type=text],#ccd-modal input[type=number],#ccd-modal select,#ccd-modal textarea{' +
    'background:var(--ccd-card);color:var(--ccd-text);border:1px solid var(--ccd-border);border-radius:6px;padding:5px 7px;font-size:13px;box-sizing:border-box;}' +
    '#ccd-modal button:hover{background:var(--ccd-hover)!important;color:var(--ccd-text)!important;}' +
    '#ccd-modal .ccd-row{display:flex;gap:8px;align-items:center;flex-wrap:wrap;margin:6px 0;}' +
    '#ccd-modal .ccd-label{font-size:12px;color:var(--ccd-secondary);min-width:72px;}' +
    '#ccd-modal .ccd-hint{font-size:12px;color:var(--ccd-muted);line-height:1.7;}' +
    '#ccd-modal .ccd-warn{font-size:12px;color:var(--ccd-amber-text);background:var(--ccd-amber-bg);border-radius:6px;padding:6px 8px;margin:6px 0;line-height:1.6;}' +
    '#ccd-modal .ccd-card{border:1px solid var(--ccd-track);border-radius:8px;padding:10px;margin-bottom:8px;background:var(--ccd-card);}' +
    '#ccd-modal .ccd-models{max-height:150px;overflow:auto;border:1px solid var(--ccd-track);border-radius:6px;padding:6px 8px;margin-top:4px;display:none;}' +
    '#ccd-modal .ccd-models label{display:block;font-size:12px;color:var(--ccd-secondary);line-height:1.9;}' +
    '#ccd-modal .ccd-pill{font-size:11px;padding:1px 7px;border-radius:999px;background:var(--ccd-blue-bg);color:var(--ccd-blue);}' +
    '#ccd-modal .ccd-pill.off{background:var(--ccd-subtle);color:var(--ccd-muted);}' +
    '#ccd-modal .ccd-seg button{font-size:12px;padding:4px 10px;border-radius:6px;border:1px solid var(--ccd-border);background:var(--ccd-subtle);color:var(--ccd-secondary);cursor:pointer;margin-right:6px;}' +
    '#ccd-modal .ccd-seg button.on{background:var(--ccd-blue-bg);color:var(--ccd-blue);border-color:var(--ccd-blue);}';
  document.head.appendChild(themeStyle);

  var cached = null;

  async function ensureAuth(force) {
    var now = Date.now() / 1000;
    if (!force && cached && cached.exp - now > 120) return cached;
    try {
      var resp = await fetch('/api/user/auth/refresh', { method: 'POST', credentials: 'include' });
      var data = await resp.json();
      if (data && data.success && data.data && data.data.access_token) {
        cached = {
          token: data.data.access_token,
          role: (data.data.user && data.data.user.role) || 0,
          uid: (data.data.user && data.data.user.id) || 0,
          exp: data.data.access_expires_at || (now + 600)
        };
        return cached;
      }
    } catch (e) { }
    cached = null; return null;
  }

  var STYLE = 'margin-left:8px;height:36px;padding:0 14px;font-size:14px;cursor:pointer;' +
    'background:var(--ccd-subtle);color:var(--ccd-secondary);border:1px solid var(--ccd-border);' +
    'border-radius:8px;transition:background .15s;';

  function styleBtn(btn, label) {
    btn.textContent = label;
    btn.style.cssText = STYLE;
    btn.onmouseenter = function () { btn.style.background = 'var(--ccd-hover)'; btn.style.color = 'var(--ccd-text)'; };
    btn.onmouseleave = function () { btn.style.background = 'var(--ccd-subtle)'; btn.style.color = 'var(--ccd-secondary)'; };
  }

  function api(auth, path, method, body) {
    return fetch(path, {
      method: method,
      headers: {
        'X-NewAPI-Token': auth.token,
        'X-NewAPI-User': String(auth.uid),
        'Content-Type': 'application/json'
      },
      body: body ? JSON.stringify(body) : undefined
    }).then(function (r) { return r.json(); });
  }

  function esc(s) {
    return String(s == null ? '' : s).replace(/[&<>"']/g, function (c) {
      return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c];
    });
  }

  // ===== 状态 =====
  var catalog = { groups: [] };
  var config = { rules: [], default_advanced: {} };
  var tokens = []; // 你名下的 key（只有 id、名字、分组、状态，没有 key 本身）
  var editing = null; // 正在编辑的规则副本；null 表示显示列表
  var advancedOpen = false;

  function groupModels(name) {
    for (var i = 0; i < (catalog.groups || []).length; i++) {
      if (catalog.groups[i].name === name) return catalog.groups[i].models || [];
    }
    return [];
  }

  function tokenByID(id) {
    for (var i = 0; i < tokens.length; i++) if (tokens[i].id === id) return tokens[i];
    return null;
  }

  function tokenLabel(t) {
    return t.name + '（' + (t.group || '默认') + (t.status === 1 ? '' : '，已停用') + '）';
  }

  function tokenOptions(sel) {
    var has = false;
    var html = tokens.map(function (t) {
      if (t.id === sel) has = true;
      return opt(String(t.id), tokenLabel(t), String(sel));
    }).join('');
    if (sel && !has) html = opt(String(sel), '（#' + sel + ' 已不存在）', String(sel)) + html;
    if (!sel) html = opt('', '— 请选择 —', '') + html;
    return html;
  }

  function langLabel(r) {
    if (r.lang === 'zh') return '中文';
    if (r.lang === 'en') return 'English';
    return '自定义：' + (r.lang_custom || '?');
  }

  function ruleSummary(r) {
    var who;
    if (r.scope === 'token') {
      var t = tokenByID(r.token_id);
      who = 'key <b>' + esc(t ? t.name : (r.token_name || ('#' + r.token_id))) + '</b> · 全部请求';
    } else {
      var scope = (r.models && r.models.length) ? r.models.length + ' 个模型' : '整个分组';
      who = '<b>' + esc(r.group) + '</b> · ' + esc(scope);
    }
    var tk = tokenByID(r.translator_token_id);
    var tname = tk ? tk.name : (r.translator_token_name || '');
    var via = r.translator_token_id ?
      ('用 key <b>' + esc(tname || ('#' + r.translator_token_id)) + '</b> 的 <b>' + esc(r.translator_model) + '</b>') :
      '<span style="color:var(--ccd-red-text)">还没选转述用的 key，请点「编辑」选一个</span>';
    return who + ' → ' + via + ' 转述成 <b>' + esc(langLabel(r)) + '</b>';
  }

  function blankRule() {
    var g = (catalog.groups[0] || {}).name || '';
    var live = tokens.filter(function (t) { return t.status === 1; });
    var t0 = live.filter(function (t) { return t.group === 'normal'; })[0] || live[0] || null;
    var tg = t0 ? t0.group : '';
    var ms = groupModels(tg);
    var d = config.default_advanced || {};
    return {
      id: '', enabled: true, scope: '', token_id: 0, token_name: '', group: g, models: [], lang: 'zh', lang_custom: '',
      translator_token_id: t0 ? t0.id : 0, translator_token_name: t0 ? t0.name : '',
      translator_group: tg, translator_model: ms.indexOf('deepseek-v4.1-flash') >= 0 ? 'deepseek-v4.1-flash' : (ms[0] || ''),
      advanced: {
        stream_first: d.stream_first, stream_step: d.stream_step, stream_max: d.stream_max,
        sync_first: d.sync_first, sync_step: d.sync_step, sync_max: d.sync_max,
        parallel: d.parallel, sync_parallel: d.sync_parallel,
        timeout_sec: d.timeout_sec, content_wait_sec: 0, extra: ''
      }
    };
  }

  function opt(v, label, selected) {
    return '<option value="' + esc(v) + '"' + (v === selected ? ' selected' : '') + '>' + esc(label) + '</option>';
  }

  function groupOptions(sel) {
    return (catalog.groups || []).map(function (g) { return opt(g.name, g.name, sel); }).join('');
  }

  function modelOptions(group, sel) {
    return groupModels(group).map(function (m) { return opt(m, m, sel); }).join('');
  }

  function renderEditor() {
    var r = editing;
    var models = groupModels(r.group);
    var checked = {};
    (r.models || []).forEach(function (m) { checked[m] = true; });
    var adv = advValues(r);
    var isTok = r.scope === 'token';
    var ttk = tokenByID(r.translator_token_id);
    var tgroup = ttk ? ttk.group : (r.translator_group || '');
    // 每个输入框用 data-adv 标明它写回 advanced 的哪个字段；值随输入实时写回 editing，
    // 不再等到保存时从 DOM 刮一遍（以前只要有一次 render 跑在输入框被移除之后，刮出来就全是 0）。
    var advBlock = advancedOpen ? (
      '<div class="ccd-card" id="ccd-adv-box" style="margin-top:8px">' +
      '<div class="ccd-row"><span class="ccd-label">流式切段</span>' +
      '<input id="ccd-sf" data-adv="stream_first" type="number" style="width:76px" value="' + esc(adv.stream_first) + '">' +
      '<input id="ccd-ss" data-adv="stream_step" type="number" style="width:76px" value="' + esc(adv.stream_step) + '">' +
      '<input id="ccd-sm" data-adv="stream_max" type="number" style="width:86px" value="' + esc(adv.stream_max) + '">' +
      '<span class="ccd-hint">首段 / 步长 / 上限（字）</span></div>' +
      '<div class="ccd-row"><span class="ccd-label">同步切段</span>' +
      '<input id="ccd-nf" data-adv="sync_first" type="number" style="width:76px" value="' + esc(adv.sync_first) + '">' +
      '<input id="ccd-ns" data-adv="sync_step" type="number" style="width:76px" value="' + esc(adv.sync_step) + '">' +
      '<input id="ccd-nm" data-adv="sync_max" type="number" style="width:86px" value="' + esc(adv.sync_max) + '">' +
      '<span class="ccd-hint">首段 / 步长 / 上限（字）</span></div>' +
      '<div class="ccd-row"><span class="ccd-label">并发</span>' +
      '<input id="ccd-p1" data-adv="parallel" type="number" style="width:70px" value="' + esc(adv.parallel) + '">' +
      '<span class="ccd-hint">流式</span>' +
      '<input id="ccd-p2" data-adv="sync_parallel" type="number" style="width:70px" value="' + esc(adv.sync_parallel) + '">' +
      '<span class="ccd-hint">同步</span></div>' +
      '<div class="ccd-row"><span class="ccd-label">单段超时</span>' +
      '<input id="ccd-to" data-adv="timeout_sec" type="number" style="width:70px" value="' + esc(adv.timeout_sec) + '">' +
      '<span class="ccd-hint">秒</span></div>' +
      '<div class="ccd-row"><span class="ccd-label">正文最多等</span>' +
      '<input id="ccd-cw" data-adv="content_wait_sec" type="number" style="width:70px" value="' + esc(adv.content_wait_sec) + '">' +
      '<span class="ccd-hint">秒，0 = 一直等（每段本来就有超时兜底）</span></div>' +
      '<div class="ccd-row" style="align-items:flex-start"><span class="ccd-label">关思考参数</span>' +
      '<textarea id="ccd-ex" data-adv="extra" style="width:100%;height:56px" placeholder="' + esc('{"reasoning_effort":"off"}') + '">' + esc(adv.extra || '') + '</textarea></div>' +
      '<div class="ccd-hint">留空用默认 {"reasoning_effort":"off","thinking":{"type":"disabled"}}；只用在转述调用上，不会碰用户的请求。</div>' +
      '<div class="ccd-warn" id="ccd-ex-err"' + (r.advancedError ? '' : ' style="display:none"') + '>' + esc(r.advancedError || '') + '</div>' +
      '</div>') : '';

    var targetRows = isTok ? (
      '<div class="ccd-row"><span class="ccd-label">key</span>' +
      '<select id="ccd-token" style="min-width:240px">' + tokenOptions(r.token_id) + '</select>' +
      '<span class="ccd-hint">用这个 key 发的请求全部套上转述，跟分组无关</span></div>'
    ) : ('' +
      '<div class="ccd-row"><span class="ccd-label">分组</span>' +
      '<select id="ccd-group" style="min-width:150px">' + groupOptions(r.group) + '</select>' +
      '<span class="ccd-hint">要转述哪个分组下的模型</span></div>' +
      '<div class="ccd-row"><span class="ccd-label">模型范围</span>' +
      '<span class="ccd-seg">' +
      '<button id="ccd-scope-all" class="' + (r.models.length ? '' : 'on') + '">整个分组</button>' +
      '<button id="ccd-scope-some" class="' + (r.models.length ? 'on' : '') + '">指定模型</button>' +
      '</span></div>' +
      '<div class="ccd-models" id="ccd-models" style="' + (r.models.length ? 'display:block' : '') + '">' +
      (models.length ? models.map(function (m) {
        return '<label><input type="checkbox" class="ccd-m" value="' + esc(m) + '"' + (checked[m] ? ' checked' : '') + '> ' + esc(m) + '</label>';
      }).join('') : '<span class="ccd-hint">这个分组现在没有启用的模型</span>') +
      '</div>');

    return '' +
      '<div class="ccd-row"><span class="ccd-label">作用对象</span><span class="ccd-seg">' +
      '<button id="ccd-target-group" class="' + (isTok ? '' : 'on') + '">分组</button>' +
      '<button id="ccd-target-token" class="' + (isTok ? 'on' : '') + '">key</button>' +
      '</span></div>' + targetRows +
      '<div class="ccd-row" style="margin-top:10px"><span class="ccd-label">转述语言</span>' +
      '<span class="ccd-seg">' +
      '<button id="ccd-lang-zh" class="' + (r.lang === 'zh' ? 'on' : '') + '">中文</button>' +
      '<button id="ccd-lang-en" class="' + (r.lang === 'en' ? 'on' : '') + '">English</button>' +
      '<button id="ccd-lang-custom" class="' + (r.lang === 'custom' ? 'on' : '') + '">自定义</button>' +
      '</span>' +
      '<input id="ccd-lang-custom-input" type="text" maxlength="40" placeholder="日语 / 法语 / Русский…" style="width:180px;' +
      (r.lang === 'custom' ? '' : 'display:none') + '" value="' + esc(r.lang_custom) + '"></div>' +
      '<div class="ccd-row"><span class="ccd-label">转述用的 key</span>' +
      '<select id="ccd-tkey" style="min-width:240px">' + tokenOptions(r.translator_token_id) + '</select>' +
      '<span class="ccd-hint">转述调用用你这个 key，用量也记在它上面</span></div>' +
      '<div class="ccd-row"><span class="ccd-label">转述模型</span>' +
      '<select id="ccd-tmodel" style="min-width:200px">' + modelOptions(tgroup, r.translator_model) + '</select>' +
      '<span class="ccd-hint">' + (tgroup ? '来自这个 key 所在的「' + esc(tgroup) + '」分组' : '先选转述用的 key') + '</span></div>' +
      '<div class="ccd-row" style="margin-top:8px">' +
      '<button id="ccd-test" style="' + STYLE + 'margin-left:0">测试转述</button>' +
      '<button id="ccd-adv-toggle" style="' + STYLE + '">' + (advancedOpen ? '收起高级设置' : '高级设置') + '</button>' +
      '<span id="ccd-test-out" class="ccd-hint"></span></div>' +
      advBlock +
      '<div class="ccd-row" style="margin-top:12px">' +
      '<button id="ccd-save" style="' + STYLE + 'margin-left:0;background:var(--ccd-blue-bg);color:var(--ccd-blue);border-color:var(--ccd-blue)">保存</button>' +
      '<button id="ccd-cancel" style="' + STYLE + '">取消</button>' +
      '<span id="ccd-save-out" class="ccd-hint"></span></div>';
  }

  function renderList() {
    var rows = (config.rules || []).map(function (r, i) {
      return '<div class="ccd-card">' +
        '<div class="ccd-row" style="margin:0">' +
        '<span class="ccd-pill' + (r.enabled ? '' : ' off') + '">' + (r.enabled ? '启用' : '停用') + '</span>' +
        '<span style="font-size:13px">' + ruleSummary(r) + '</span>' +
        '<span style="margin-left:auto"></span>' +
        '<button data-act="toggle" data-i="' + i + '" style="' + STYLE + 'margin-left:0;height:28px;font-size:12px">' + (r.enabled ? '停用' : '启用') + '</button>' +
        '<button data-act="edit" data-i="' + i + '" style="' + STYLE + 'margin-left:0;height:28px;font-size:12px">编辑</button>' +
        '<button data-act="del" data-i="' + i + '" style="' + STYLE + 'margin-left:0;height:28px;font-size:12px">删除</button>' +
        '</div></div>';
    }).join('');
    if (!rows) {
      rows = '<div class="ccd-hint" style="padding:8px 0">还没有规则。点「新建规则」给某个分组或某个 key 套上思维链转述。</div>';
    }
    return '<div class="ccd-row">' +
      '<button id="ccd-new" style="' + STYLE + 'margin-left:0">新建规则</button>' +
      '<button id="ccd-reload" style="' + STYLE + '">刷新</button>' +
      '<span class="ccd-hint">规则可以按分组配，也可以直接选你的某个 key；转述用的 key 也从你已有的 key 里选</span>' +
      '</div>' + rows;
  }

  function render() {
    var box = document.getElementById('ccd-body');
    if (!box) return;
    box.innerHTML = editing ? renderEditor() : renderList();
    if (editing) bindEditor(); else bindList();
  }

  function bindList() {
    var box = document.getElementById('ccd-body');
    var n = document.getElementById('ccd-new');
    if (n) n.onclick = function () { editing = blankRule(); advancedOpen = false; render(); };
    var rl = document.getElementById('ccd-reload');
    if (rl) rl.onclick = function () { loadAll(true); };
    box.querySelectorAll('button[data-act]').forEach(function (b) {
      b.onclick = function () {
        var i = parseInt(b.getAttribute('data-i'), 10);
        var act = b.getAttribute('data-act');
        if (act === 'toggle') {
          config.rules[i].enabled = !config.rules[i].enabled;
          save(config.rules);
        } else if (act === 'del') {
          if (!confirm('删除这条规则？')) return;
          var rules = config.rules.slice();
          rules.splice(i, 1);
          save(rules);
        } else if (act === 'edit') {
          editing = JSON.parse(JSON.stringify(config.rules[i]));
          advancedOpen = false;
          render();
        }
      };
    });
  }

  function bindEditor() {
    var r = editing;
    var g = document.getElementById('ccd-group');
    if (g) g.onchange = function () {
      r.group = g.value;
      r.models = [];
      render();
    };
    var all = document.getElementById('ccd-scope-all'), some = document.getElementById('ccd-scope-some');
    if (all) all.onclick = function () { r.models = []; render(); };
    if (some) some.onclick = function () {
      var box = document.getElementById('ccd-models');
      box.style.display = 'block';
      all.classList.remove('on'); some.classList.add('on');
    };
    var tk = document.getElementById('ccd-tkey');
    if (tk) tk.onchange = function () {
      collectEditor();
      var ms = groupModels(r.translator_group);
      if (ms.indexOf(r.translator_model) < 0) r.translator_model = ms[0] || '';
      render();
    };
    var tgtG = document.getElementById('ccd-target-group'), tgtT = document.getElementById('ccd-target-token');
    if (tgtG) tgtG.onclick = function () {
      collectEditor();
      r.scope = '';
      if (!r.group) r.group = (catalog.groups[0] || {}).name || '';
      render();
    };
    if (tgtT) tgtT.onclick = function () {
      collectEditor();
      r.scope = 'token';
      r.models = [];
      if (!r.token_id && tokens.length) { r.token_id = tokens[0].id; r.token_name = tokens[0].name; }
      render();
    };
    var tok = document.getElementById('ccd-token');
    if (tok) tok.onchange = function () { collectEditor(); render(); };
    var tm = document.getElementById('ccd-tmodel');
    if (tm) tm.onchange = function () { r.translator_model = tm.value; };
    ['zh', 'en', 'custom'].forEach(function (l) {
      var b = document.getElementById('ccd-lang-' + l);
      if (b) b.onclick = function () {
        r.lang = l;
        render();
      };
    });
    var ci = document.getElementById('ccd-lang-custom-input');
    if (ci) ci.oninput = function () { r.lang_custom = ci.value; };
    var adv = document.getElementById('ccd-adv-toggle');
    if (adv) adv.onclick = function () {
      // 值已经随输入写回 editing 了，这里只切换展开状态，不用再刮一遍 DOM
      if (advancedOpen && editing.advancedError) return; // 填错了就别收起，提示留在眼前
      advancedOpen = !advancedOpen;
      render();
    };
    // 高级设置的每个输入框随输入立刻写回 editing.advanced —— 保存时直接用它，
    // 不再依赖「保存那一刻输入框还在 DOM 里」这件事。
    function writeAdv(el) {
      var k = el.getAttribute('data-adv');
      var res = applyAdvanced(r, k, el.value);
      if (k !== 'extra') return;
      r.advancedError = res.ok ? '' : res.message;
      var err = document.getElementById('ccd-ex-err');
      if (err) {
        err.textContent = r.advancedError;
        err.style.display = r.advancedError ? '' : 'none';
      }
    }
    document.querySelectorAll('#ccd-adv-box [data-adv]').forEach(function (el) {
      el.oninput = function () { writeAdv(el); };
      el.onchange = function () { writeAdv(el); };
    });
    var t = document.getElementById('ccd-test');
    if (t) t.onclick = function () { runTest(t); };
    var s = document.getElementById('ccd-save');
    if (s) s.onclick = function () { collectEditor(); saveRule(s); };
    var c = document.getElementById('ccd-cancel');
    if (c) c.onclick = function () { editing = null; render(); };
    var box = document.getElementById('ccd-models');
    if (box) box.onclick = function (e) {
      if (e.target && e.target.classList && e.target.classList.contains('ccd-m')) {
        var v = e.target.value, on = e.target.checked;
        var arr = (r.models || []).filter(function (x) { return x !== v; });
        if (on) arr.push(v);
        r.models = arr;
      }
    };
  }

  // advValues 把规则上的高级参数补成可显示的值：没存过的（0 / 缺失）就用服务端给的默认值，
  // 免得刚点开「高级设置」就是一片 0。
  function advValues(r) {
    var d = config.default_advanced || {};
    var a = (r && r.advanced) || {};
    var pick = function (k) {
      var v = parseInt(a[k], 10);
      if (v > 0) return v;
      v = parseInt(d[k], 10);
      return v > 0 ? v : 0;
    };
    var cw = a.content_wait_sec == null ? (parseInt(d.content_wait_sec, 10) || 0) : (parseInt(a.content_wait_sec, 10) || 0);
    var ex = a.extra;
    if (ex && typeof ex === 'object') ex = JSON.stringify(ex); // 接口回的是对象，输入框只认文本
    if (ex == null) ex = '';
    return {
      stream_first: pick('stream_first'), stream_step: pick('stream_step'), stream_max: pick('stream_max'),
      sync_first: pick('sync_first'), sync_step: pick('sync_step'), sync_max: pick('sync_max'),
      parallel: pick('parallel'), sync_parallel: pick('sync_parallel'),
      timeout_sec: pick('timeout_sec'), content_wait_sec: cw, extra: String(ex)
    };
  }

  // applyAdvanced 把高级设置里某个输入框的值写到规则上。
  // 输入框一改就调它，所以保存时规则里已经是最新值 —— 不必再在保存那一刻去 DOM 里刮一遍
  // （高级设置收起后输入框不在文档里，刮出来的全是 0，这正是「保存后重进设置全变 0」的原因）。
  // 返回 {ok, message}：只有关思考参数填错时会 ok=false。
  function applyAdvanced(r, key, raw) {
    if (!r) return { ok: true };
    if (!r.advanced) r.advanced = {};
    if (key === 'extra') {
      var p = parseExtra(raw);
      if (!p.ok) return p;
      r.advanced.extra = p.value;
      return { ok: true };
    }
    var v = parseInt(raw, 10);
    // 清空或填了非数字就记 0，交给后端 clamp 回默认值；沿用旧值会让界面和实际不符
    r.advanced[key] = isNaN(v) ? 0 : v;
    return { ok: true };
  }

  // parseExtra 校验「关思考参数」：留空表示用默认，填了就必须是 JSON 对象。
  // 和后端一样收两种写法：对象本身，或者把对象当字符串包一层。
  function parseExtra(text) {
    var s = String(text == null ? '' : text).trim();
    if (s === '') return { ok: true, value: '' };
    var bad = { ok: false, message: '关思考参数不是合法的 JSON，要一个对象，例如 {"reasoning_effort":"off"}' };
    var v;
    try { v = JSON.parse(s); } catch (e) { return { ok: false, message: '关思考参数不是合法的 JSON：' + e.message }; }
    if (typeof v === 'string') { // 整个被当成字符串包了一层，解开再解析一次
      try { v = JSON.parse(v.trim()); } catch (e) { return bad; }
    }
    if (!v || typeof v !== 'object' || Object.prototype.toString.call(v) === '[object Array]') return bad;
    return { ok: true, value: JSON.stringify(v) };
  }

  function collectEditor() {
    var r = editing;
    if (!r) return;
    var mods = [];
    document.querySelectorAll('#ccd-models .ccd-m').forEach(function (c) { if (c.checked) mods.push(c.value); });
    if (document.getElementById('ccd-models') && document.getElementById('ccd-models').style.display !== 'none') {
      r.models = mods;
    }
    var gv = document.getElementById('ccd-group');
    if (gv) r.group = gv.value;
    var tks = document.getElementById('ccd-tkey'), tms = document.getElementById('ccd-tmodel');
    if (tks) {
      var tt = tokenByID(parseInt(tks.value, 10) || 0);
      r.translator_token_id = tt ? tt.id : 0;
      r.translator_token_name = tt ? tt.name : '';
      r.translator_group = tt ? tt.group : '';
    }
    if (tms) r.translator_model = tms.value;
    var tok = document.getElementById('ccd-token');
    if (r.scope === 'token') {
      if (tok) {
        var t2 = tokenByID(parseInt(tok.value, 10) || 0);
        r.token_id = t2 ? t2.id : 0;
        r.token_name = t2 ? t2.name : '';
      }
      r.models = [];
    }
    var ci = document.getElementById('ccd-lang-custom-input');
    if (ci) { r.lang_custom = ci.value; if (r.lang === 'custom') { } }
    // 高级设置不在这里读：每个输入框一改就已经写回 r.advanced 了。
    // 以前是保存时统一刮 DOM，高级设置一收起（输入框不在 DOM 里）就全读成 0，
    // 于是「保存之后重进，之前设过的全变 0」。
  }

  async function saveRule(btn) {
    var out = document.getElementById('ccd-save-out');
    var auth = await ensureAuth(false);
    if (!auth || auth.role < 100) { out.textContent = '会话失效，请刷新页面'; return; }
    if (editing && editing.advancedError) { out.textContent = '❌ ' + editing.advancedError; return; }
    var r = JSON.parse(JSON.stringify(editing));
    delete r.advancedError;
    r.enabled = true; // 在编辑器里保存 = 要用这条规则（没选转述 key 而被停用的规则，补好后保存即启用）
    if (r.advanced && (r.advanced.extra === '' || r.advanced.extra == null)) delete r.advanced.extra;
    var rules = (config.rules || []).slice();
    var idx = -1;
    for (var i = 0; i < rules.length; i++) if (rules[i].id && rules[i].id === r.id) idx = i;
    if (idx >= 0) rules[idx] = r; else rules.push(r);
    btn.disabled = true; out.textContent = '保存中…';
    var resp = await api(auth, '/ccd/admin/config', 'PUT', { rules: rules });
    btn.disabled = false;
    if (!resp.success) { out.textContent = '❌ ' + (resp.message || '保存失败'); return; }
    editing = null;
    await loadAll(false);
    var o2 = document.getElementById('ccd-save-out');
    if (o2) o2.textContent = '✅ 已保存';
  }

  async function runTest(btn) {
    var out = document.getElementById('ccd-test-out');
    var auth = await ensureAuth(false);
    if (!auth || auth.role < 100) { out.textContent = '会话失效，请刷新页面'; return; }
    if (editing && editing.advancedError) { out.textContent = '❌ ' + editing.advancedError; return; }
    var r = JSON.parse(JSON.stringify(editing));
    delete r.advancedError;
    if (r.advanced && (r.advanced.extra === '' || r.advanced.extra == null)) delete r.advanced.extra;
    btn.disabled = true; out.textContent = '测试中…';
    var resp = await api(auth, '/ccd/admin/test', 'POST', {
      lang: r.lang, lang_custom: r.lang_custom,
      translator_token_id: r.translator_token_id, translator_model: r.translator_model,
      advanced: r.advanced
    });
    btn.disabled = false;
    if (!resp.success) { out.innerHTML = '<span style="color:var(--ccd-red-text)">❌ ' + esc(resp.message || '失败') + '</span>'; return; }
    var d = resp.data || {};
    var think = (d.reasoning_tokens || 0) > 0 ? ('⚠️ 转述模型偷偷思考了 ' + d.reasoning_tokens + ' token') : '没有偷偷思考';
    out.innerHTML = '<span style="color:var(--ccd-green-text)">✅ ' + d.ms + 'ms，' + think + '</span>' +
      '<div class="ccd-card" style="margin-top:6px;white-space:pre-wrap;font-size:13px;color:var(--ccd-text)">' + esc(d.text || '（空）') + '</div>';
  }

  async function save(rules) {
    var auth = await ensureAuth(false);
    if (!auth || auth.role < 100) { alert('会话失效，请刷新页面'); return; }
    var clean = JSON.parse(JSON.stringify(rules)).map(function (r) {
      delete r.advancedError;
      if (r.advanced && (r.advanced.extra === '' || r.advanced.extra == null)) delete r.advanced.extra;
      return r;
    });
    var resp = await api(auth, '/ccd/admin/config', 'PUT', { rules: clean });
    if (!resp.success) { alert('❌ ' + (resp.message || '保存失败')); return; }
    await loadAll(false);
  }

  async function loadAll(force) {
    var auth = await ensureAuth(force);
    var body = document.getElementById('ccd-body');
    if (!auth || auth.role < 100) {
      if (body) body.innerHTML = '<div class="ccd-hint" style="color:var(--ccd-red-text)">会话失效，请刷新页面</div>';
      return;
    }
    if (body) body.innerHTML = '<div class="ccd-hint">加载中…</div>';
    try {
      var both = await Promise.all([
        api(auth, '/ccd/admin/catalog', 'GET'),
        api(auth, '/ccd/admin/config', 'GET'),
        api(auth, '/ccd/admin/tokens', 'GET')
      ]);
      if (both[0].success) catalog = both[0].data || { groups: [] };
      if (both[1].success) config = both[1].data || { rules: [] };
      tokens = (both[2].success && both[2].data) || [];
      if (!both[0].success || !both[1].success || !both[2].success) {
        if (body) body.innerHTML = '<div class="ccd-hint" style="color:var(--ccd-red-text)">' +
          esc((both[0].message || both[1].message || both[2].message || '加载失败')) + '</div>';
        return;
      }
      render();
    } catch (e) {
      if (body) body.innerHTML = '<div class="ccd-hint" style="color:var(--ccd-red-text)">请求失败：' + esc(String(e)) + '</div>';
    }
  }

  function openModal() {
    if (document.getElementById(MODAL)) return;
    var mask = document.createElement('div');
    mask.id = MODAL;
    mask.style.cssText = 'position:fixed;inset:0;background:var(--ccd-overlay);z-index:9999;display:flex;align-items:center;justify-content:center;';
    mask.innerHTML =
      '<div style="background:var(--ccd-surface);border:1px solid var(--ccd-border);border-radius:12px;padding:18px;' +
      'height:88vh;width:min(94vw, 760px);box-sizing:border-box;overflow:auto;scrollbar-width:thin;' +
      'color:var(--ccd-text);box-shadow:var(--ccd-shadow)">' +
      '<div style="display:flex;align-items:center;margin-bottom:10px">' +
      '<b style="font-size:16px">思维链转述</b>' +
      '<button id="ccd-close" style="margin-left:auto;' + STYLE + 'margin-left:auto">关闭</button></div>' +
      '<div class="ccd-hint" style="margin-bottom:10px">客户端看到的思维链文字会被换成转述；正文、工具调用、用量、签名等一律不动。' +
      '支持 <b>/v1/chat/completions</b> 与 <b>/v1/messages</b>（流式/同步）；' +
      '<b>/v1/responses</b>、Gemini 原生、embeddings / rerank 本期原样透传、不转述。</div>' +
      '<div id="ccd-body"></div></div>';
    document.body.appendChild(mask);
    document.getElementById('ccd-close').onclick = function () { editing = null; mask.remove(); };
    mask.onclick = function (e) { if (e.target === mask) { editing = null; mask.remove(); } };
    loadAll(true);
  }

  // 日志页工具栏里 new-api 自带的按钮，按优先级找一个可见的，把我们的按钮插在它右边。
  var ANCHOR_LABELS = ['查看', '搜索', '查询', 'Search', 'Query'];

  function findAnchor() {
    var btns = document.querySelectorAll('button');
    for (var n = 0; n < ANCHOR_LABELS.length; n++) {
      for (var i = 0; i < btns.length; i++) {
        var b = btns[i];
        if (b.id !== BTN && b.offsetParent !== null && b.textContent.trim() === ANCHOR_LABELS[n]) return b;
      }
    }
    return null;
  }

  // 找到工具栏就插在按钮右边；找不到（版本差异或页面还没渲染完）就先放成右下角的悬浮按钮，之后找到了再挪过去。
  function placeBtn(btn, anchor) {
    if (anchor) {
      styleBtn(btn, '思维链转述');
      btn.removeAttribute('data-floating');
      anchor.parentNode.insertBefore(btn, anchor.nextSibling);
      return;
    }
    styleBtn(btn, '思维链转述');
    btn.style.position = 'fixed';
    btn.style.right = '24px';
    btn.style.bottom = '24px';
    btn.style.zIndex = '9998';
    btn.style.boxShadow = 'var(--ccd-shadow)';
    btn.setAttribute('data-floating', '1');
    document.body.appendChild(btn);
  }

  async function tick() {
    var onLogsPage = /\/(log|usage-logs|task-logs)/i.test(location.pathname);
    var btn = document.getElementById(BTN);
    if (!onLogsPage) { if (btn) btn.remove(); return; }
    var auth = await ensureAuth(false);
    if (!(auth && auth.role >= 100)) { if (btn) btn.remove(); return; }
    var anchor = findAnchor();
    if (btn) {
      if (btn.getAttribute('data-floating') && anchor) placeBtn(btn, anchor);
      return;
    }
    btn = document.createElement('button');
    btn.id = BTN;
    btn.onclick = openModal;
    placeBtn(btn, anchor);
  }
  setInterval(tick, 3000);
  tick();
})();
