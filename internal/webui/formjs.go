package webui

import (
	"net/http"
)

// formJS 是给插件设置页用的通用表单组件：喂 schema，自动渲染控件并提交。
const formJS = `/* QixinBot 通用设置表单
 * 用法：
 *   <script src="/ui/qixin-form.js"></script>
 *   <div id="form"></div>
 *   <script>QixinForm.mount('#form', 'weather')</script>
 */
(function () {
  'use strict';

  function h(tag, attrs, children) {
    var el = document.createElement(tag);
    if (attrs) {
      for (var k in attrs) {
        if (k === 'class') el.className = attrs[k];
        else if (k === 'text') el.textContent = attrs[k];
        else if (k.indexOf('on') === 0) el.addEventListener(k.slice(2), attrs[k]);
        else el.setAttribute(k, attrs[k]);
      }
    }
    (children || []).forEach(function (c) {
      el.appendChild(typeof c === 'string' ? document.createTextNode(c) : c);
    });
    return el;
  }

  function control(item, value) {
    switch (item.type) {
      case 'boolean': {
        var box = h('input', { type: 'checkbox', name: item.key, id: 'f_' + item.key });
        box.checked = value === true || value === 'true';
        return h('label', { class: 'switch', style: 'display:flex;align-items:center;gap:8px;margin:6px 0 14px' }, [box, item.label || item.key]);
      }
      case 'select': {
        var sel = h('select', { name: item.key, id: 'f_' + item.key });
        (item.options || []).forEach(function (opt) {
          var o = h('option', { value: opt }, [opt]);
          if (String(value) === String(opt)) o.selected = true;
          sel.appendChild(o);
        });
        return sel;
      }
      case 'number': {
        var num = h('input', { type: 'number', name: item.key, id: 'f_' + item.key });
        num.value = value === undefined || value === null ? '' : value;
        if (item.min !== undefined) num.min = item.min;
        if (item.max !== undefined) num.max = item.max;
        return num;
      }
      case 'password': {
        var pwd = h('input', { type: 'password', name: item.key, id: 'f_' + item.key, autocomplete: 'new-password' });
        pwd.value = value === undefined || value === null ? '' : value;
        return pwd;
      }
      default: {
        var txt = h('input', { type: 'text', name: item.key, id: 'f_' + item.key });
        txt.value = value === undefined || value === null ? '' : value;
        return txt;
      }
    }
  }

  async function mount(selector, pluginId, opts) {
    opts = opts || {};
    var root = typeof selector === 'string' ? document.querySelector(selector) : selector;
    if (!root) return;

    var res;
    try {
      res = await fetch('/api/plugin/' + encodeURIComponent(pluginId) + '/settings');
    } catch (e) {
      root.textContent = '加载设置失败：' + e.message;
      return;
    }
    if (!res.ok) {
      root.textContent = '加载设置失败：HTTP ' + res.status;
      return;
    }

    var data = await res.json();
    var schema = data.schema || [];
    var values = data.values || {};

    if (!schema.length) {
      root.textContent = '该插件没有可配置项。';
      return;
    }

    var form = h('form', { class: 'qixin-form' });
    schema.forEach(function (item) {
      var label = h('label', { for: 'f_' + item.key, style: 'display:block;font-size:12px;color:#666;margin:12px 0 4px' }, [item.label || item.key]);
      form.appendChild(label);
      var ctrl = control(item, values[item.key]);
      ctrl.style.width = '100%';
      ctrl.style.padding = '6px 9px';
      ctrl.style.border = '1px solid var(--input-border)';
      ctrl.style.borderRadius = '4px';
      ctrl.style.fontSize = '13px';
      form.appendChild(ctrl);
    });

    var msg = h('span', { style: 'margin-left:10px;font-size:12px;color:var(--ok)' }, ['']);
    var btn = h('button', { type: 'submit', style: 'margin-top:16px;padding:7px 18px;background:var(--accent);color:var(--on-accent);border:0;border-radius:4px;cursor:pointer' }, ['保存']);

    form.appendChild(h('div', { style: 'margin-top:6px' }, [btn, msg]));

    form.addEventListener('submit', async function (e) {
      e.preventDefault();
      var body = new URLSearchParams(new FormData(form));
      // 未勾选的 checkbox 不会出现在 FormData 里，手动补 false
      schema.forEach(function (item) {
        if (item.type === 'boolean' && !body.has(item.key)) body.set(item.key, 'false');
      });
      msg.textContent = '保存中…';
      msg.style.color = '#666';
      try {
        var r = await fetch('/api/plugin/' + encodeURIComponent(pluginId) + '/settings', { method: 'POST', body: body });
        if (r.ok || r.redirected) {
          msg.textContent = '已保存';
          msg.style.color = 'var(--ok)';
        } else {
          msg.textContent = '保存失败 HTTP ' + r.status;
          msg.style.color = 'var(--bad)';
        }
      } catch (err) {
        msg.textContent = '保存失败：' + err.message;
        msg.style.color = 'var(--bad)';
      }
    });

    root.innerHTML = '';
    root.appendChild(form);
    if (opts.after) opts.after(form, data);
  }

  window.QixinForm = { mount: mount };
})();
`

func (s *Server) handleFormJS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_, _ = w.Write([]byte(formJS))
}
