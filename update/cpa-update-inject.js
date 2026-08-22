(function () {
  'use strict';
  // CPA 一键更新 — 按钮放在"系统概览"卡片内（版本行下方），紧凑小按钮；检测到已是最新时禁用
  var KEY = null;
  var STATUS = null; // {hasUpdate, kernel, manager}

  // ---- 解密面板 localStorage 中的管理密钥 (enc::v1:: + XOR) ----
  function decryptStored(v) {
    try {
      if (!v) return null;
      if (typeof v !== 'string') v = String(v);
      if (v.indexOf('enc::v1::') === 0) {
        var Dn = 'cli-proxy-api-webui::secure-storage';
        var keyStr = Dn + '|' + window.location.host + '|' + navigator.userAgent;
        var key = new TextEncoder().encode(keyStr);
        var raw = atob(v.slice(9));
        var bytes = new Uint8Array(raw.length);
        for (var i = 0; i < raw.length; i++) bytes[i] = raw.charCodeAt(i);
        var out = new Uint8Array(bytes.length);
        for (var i = 0; i < bytes.length; i++) out[i] = bytes[i] ^ key[i % key.length];
        return new TextDecoder().decode(out);
      }
      return v;
    } catch (e) { return null; }
  }

  function readKeyFromStorage() {
    try {
      var s = localStorage.getItem('managementKey');
      if (s) {
        var d = decryptStored(s);
        if (d) return 'Bearer ' + d;
      }
    } catch (e) {}
    return null;
  }

  // ---- 兜底：捕获面板请求头中的 Authorization ----
  function hook() {
    try {
      var origFetch = window.fetch;
      if (origFetch) {
        window.fetch = function (input, init) {
          try {
            var h = (init && init.headers) || {};
            var a = h.Authorization || h.authorization;
            if (!a && typeof Headers !== 'undefined' && h instanceof Headers) a = h.get('Authorization');
            if (a && a.indexOf('Bearer ') === 0) KEY = a;
          } catch (e) {}
          return origFetch.apply(this, arguments);
        };
      }
      var origSet = XMLHttpRequest.prototype.setRequestHeader;
      XMLHttpRequest.prototype.setRequestHeader = function (k, v) {
        try {
          if (String(k).toLowerCase() === 'authorization' && String(v).indexOf('Bearer ') === 0) KEY = String(v);
        } catch (e) {}
        return origSet.apply(this, arguments);
      };
    } catch (e) {}
  }

  function ensureKey() {
    if (KEY) return KEY;
    var k = readKeyFromStorage();
    if (k) { KEY = k; return k; }
    return null;
  }

  // ---- 查询版本状态（服务器 /version-status，带密钥）----
  function fetchStatus(cb) {
    var auth = ensureKey();
    if (!auth) { cb(null); return; }
    fetch('/cpa-update/version-status', { headers: { 'Authorization': auth } })
      .then(function (r) { return r.json(); })
      .then(function (j) { cb(j); })
      .catch(function () { cb(null); });
  }

  // ---- 在"系统概览"卡片内插入小按钮 ----
  function findSystemOverviewGrid() {
    var all = document.querySelectorAll('div,span');
    for (var i = 0; i < all.length; i++) {
      var el = all[i];
      if (el.childElementCount !== 0) continue;
      var t = (el.textContent || '').trim();
      if (t !== '管理面板版本' && t !== 'Management Panel Version' && t !== 'App Version') continue;
      var p = el.parentElement;
      for (var d = 0; p && d < 6; d++, p = p.parentElement) {
        var cls = p.className || '';
        if (typeof cls === 'string' && cls.indexOf('systemGrid') !== -1) return p;
      }
      return el.parentElement.parentElement;
    }
    return null;
  }

  function updateBtnState(b) {
    if (!STATUS) { b.disabled = true; b.title = '正在检测版本...'; b.style.opacity = '.55'; return; }
    if (STATUS.hasUpdate) {
      b.disabled = false;
      b.title = '检测到新版本，点击更新';
      b.style.opacity = '1';
      b.textContent = '↻ 一键更新';
    } else {
      b.disabled = true;
      b.title = '当前已是最新版本';
      b.style.opacity = '.45';
      b.textContent = '✓ 已是最新';
    }
  }

  function insertBtn() {
    if (document.getElementById('cpa-update-btn')) return true;
    var grid = findSystemOverviewGrid();
    if (!grid) return false;
    var row = document.createElement('div');
    row.className = 'cpa-update-row';
    row.style.cssText = 'grid-column:1/-1;margin-top:4px;display:flex;justify-content:flex-end;';
    var b = document.createElement('button');
    b.id = 'cpa-update-btn';
    b.type = 'button';
    b.textContent = '↻ 一键更新';
    b.style.cssText = 'padding:4px 10px;border:none;border-radius:6px;background:#4c8dff;color:#fff;font-size:12px;font-weight:600;line-height:1.4;cursor:pointer;box-shadow:none;';
    b.onclick = function (e) {
      e.stopPropagation();
      if (b.disabled) return;
      doUpdate(b);
    };
    row.appendChild(b);
    grid.appendChild(row);
    updateBtnState(b);
    fetchStatus(function (s) { STATUS = s; updateBtnState(b); });
    return true;
  }

  function ensureBtn() {
    if (document.getElementById('cpa-update-btn')) return;
    insertBtn();
  }

  // ---- 更新流程 ----
  function showMsg(txt, ok) {
    var existing = document.getElementById('cpa-update-msg');
    var d = existing || document.createElement('div');
    if (!existing) {
      d.id = 'cpa-update-msg';
      d.style.cssText = 'position:fixed;right:20px;bottom:110px;z-index:100000;max-width:440px;max-height:340px;overflow:auto;padding:12px 14px;border-radius:10px;background:#1f2937;color:#fff;font:12px/1.5 monospace;white-space:pre-wrap;box-shadow:0 8px 24px rgba(0,0,0,.35);';
      document.body.appendChild(d);
    }
    d.style.background = ok ? '#1f2937' : '#7f1d1d';
    d.textContent = txt;
    return d;
  }

  function doUpdate(btn) {
    var auth = ensureKey();
    if (!auth) {
      alert('未找到管理密钥：请先在面板右上角登录（输入管理密钥），刷新页面后再点一键更新。');
      return;
    }
    if (!confirm('确定执行 CPA 一键更新？\n将备份配置 → docker compose pull → 重建容器。\n期间服务会短暂重启（约 1-2 分钟）。')) return;
    btn.disabled = true;
    btn.textContent = '更新中...';
    var box = showMsg('正在触发更新...', true);
    fetch('/cpa-update/update', {
      method: 'POST',
      headers: { 'Authorization': auth, 'Content-Type': 'application/json' }
    }).then(function (r) { return r.json(); }).then(function (j) {
      if (j.ok) {
        box.textContent = '更新已开始，等待完成...\n' + (j.message || '');
        var timer = setInterval(function () {
          fetch('/cpa-update/update', { headers: { 'Authorization': auth } })
            .then(function (r) { return r.json(); })
            .then(function (s) {
              box.textContent = s.log || '...';
              if (!s.running) {
                clearInterval(timer);
                btn.textContent = '↻ 一键更新';
                fetchStatus(function (st) { STATUS = st; updateBtnState(btn); });
                box.style.background = '#065f46';
                box.textContent = '✅ 更新流程已结束（可刷新页面确认新版本）\n\n' + (s.log || '');
                setTimeout(function () { try { location.reload(); } catch (e) {} }, 5000);
              }
            }).catch(function () {});
        }, 5000);
      } else {
        btn.disabled = false;
        btn.textContent = '↻ 一键更新';
        box.style.background = '#7f1d1d';
        box.textContent = '更新触发失败：' + (j.error || JSON.stringify(j));
      }
    }).catch(function (e) {
      btn.disabled = false;
      btn.textContent = '↻ 一键更新';
      box.style.background = '#7f1d1d';
      box.textContent = '请求失败：' + e;
    });
  }

  // ---- 启动 ----
  hook();
  if (window.MutationObserver) {
    var mo = new MutationObserver(function () { ensureBtn(); });
    mo.observe(document.documentElement, { childList: true, subtree: true });
  }
  if (document.body) ensureBtn();
  else window.addEventListener('DOMContentLoaded', ensureBtn);
})();
