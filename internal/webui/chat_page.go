package webui

import (
	"context"
	"encoding/base64"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/xin/onebot/internal/chatbuf"
	"github.com/xin/onebot/internal/render"
)

// pageChat 渲染人工聊天面板：左边会话列表，右边消息流 + 输入框。
func (s *Server) pageChat(w http.ResponseWriter, r *http.Request) {
	var b strings.Builder
	b.WriteString(`<h1>聊天</h1><p class="sub">以机器人身份发言，或观察收到的消息。只保留最近 500 条，重启清空。<br>想测插件？点左边的「虚拟群」——你在那里说话会当成普通群员，插件的回复不会发到真实 QQ。</p>`)

	b.WriteString(`<style>
.cw{display:flex;gap:12px;height:560px}
.cw-side{width:230px;flex:0 0 230px;border:1px solid var(--border);border-radius:10px;background:var(--panel);overflow:auto}
.cw-side-search{padding:8px;border-bottom:1px solid var(--border);position:sticky;top:0;background:var(--panel);z-index:1}
.cw-side-search input[type=text]{width:100%}
.cw-toggle{margin-top:6px;font-size:12px;color:var(--muted)}
.cw-toggle label{cursor:pointer;user-select:none}
.cw-item{padding:9px 12px;border-bottom:1px solid var(--border);cursor:pointer;font-size:13px}
.cw-item:hover{background:var(--hover)}
.cw-item.active{background:var(--hover);box-shadow:inset 3px 0 0 var(--accent)}
.cw-item-top{display:flex;justify-content:space-between;gap:6px;align-items:baseline}
.cw-item-name{font-weight:600;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.cw-item-time{font-size:10px;color:var(--muted);flex:0 0 auto}
.cw-item-last{font-size:11px;color:var(--muted);margin-top:2px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.cw-main{flex:1;min-width:0;display:flex;flex-direction:column;border:1px solid var(--border);border-radius:10px;background:var(--panel)}
.cw-msgs{flex:1;overflow:auto;padding:16px}
.cw-row{margin-bottom:14px}
.cw-row.out{text-align:right}
.cw-meta{font-size:11px;color:var(--muted);margin-bottom:4px}
.cw-bubble{display:inline-block;padding:8px 12px;border-radius:10px;background:var(--hover);max-width:70%;text-align:left;white-space:pre-wrap;word-break:break-word;line-height:1.5}
.cw-row.out .cw-bubble{background:var(--accent);color:var(--on-accent)}
.cw-img{display:block;max-width:220px;max-height:220px;border-radius:8px;margin-top:6px;cursor:zoom-in;object-fit:cover}
.cw-row.out .cw-img{margin-left:auto}
.cw-form{display:flex;gap:10px;padding:12px;border-top:1px solid var(--border)}
.cw-form input[type=text]{flex:1;min-width:0}
.cw-form button{flex:0 0 auto;padding:8px 22px}
</style>`)

	b.WriteString(`<div class="cw">
  <div class="cw-side">
    <div class="cw-side-search">
      <input id="cw-search" type="text" placeholder="搜索会话…" autocomplete="off">
      <div class="cw-toggle"><label><input id="cw-all" type="checkbox"> 显示全部会话</label></div>
    </div>
    <div id="cw-virtual" class="cw-item" data-type="virtual" data-id="0" style="border-bottom:2px solid var(--border)">
      <div class="cw-item-top"><span class="cw-item-name">🧪 虚拟群</span><span class="cw-item-time">测试</span></div>
      <div class="cw-item-last muted">在这里说话，插件回你不发到 QQ</div>
    </div>
    <div id="cw-list"></div>
  </div>
  <div class="cw-main">
    <div id="cw-msgs" class="cw-msgs"><div class="muted" style="padding:20px">左边选一个会话。</div></div>
    <div id="cw-identity-bar" style="display:none;padding:8px 12px;border-top:1px solid var(--border);font-size:12px;color:var(--muted)">
      以
      <select id="cw-identity" style="padding:3px 8px;margin:0 4px">
        <option value="owner">主人</option>
        <option value="admin">群管理</option>
        <option value="member">纯路人</option>
      </select>
      的身份说话
    </div>
    <form id="cw-form" class="cw-form">
      <input id="cw-text" type="text" placeholder="说点什么…" autocomplete="off">
      <button class="primary" type="submit">发送</button>
    </form>
  </div>
</div>`)

	b.WriteString(`<script>
(function () {
  const list = document.getElementById('cw-list');
  const msgs = document.getElementById('cw-msgs');
  const text = document.getElementById('cw-text');
  const form = document.getElementById('cw-form');
  const search = document.getElementById('cw-search');
  const showAllBox = document.getElementById('cw-all');

  let pluginNames = new Map();   // id -> 中文名
  let nameCount = new Map();     // 中文名 -> 出现次数（用于重名时加 id）
  let contacts = [];
  let byId = new Map();     // id -> message，用 Map 才能在 plugin 变化时原地更新
  let after = 0;
  let filter = null;

  // 虚拟群：消息只存在浏览器内存里，绝不落库、绝不发 QQ
  const virtualId = 'virtual:0';
  let virtualMsgs = [];   // {dir:'in'|'out', text, ts, plugin}

  function allMsgs() { return Array.from(byId.values()).sort((a, b) => a.id - b.id); }

  async function loadPlugins() {
    try {
      const r = await fetch('/api/overview');
      const data = await r.json();
      pluginNames.clear(); nameCount.clear();
      for (const p of data.plugins || []) {
        pluginNames.set(p.id, p.name || p.id);
        nameCount.set(p.name || p.id, (nameCount.get(p.name || p.id) || 0) + 1);
      }
    } catch (e) {}
  }

  // 插件显示名：优先中文名；重名时补 (id) 区分；内置命令显示「框架」
  function pluginLabel(id) {
    if (!id) return '';
    if (id === 'builtin') return '框架';
    if (id === 'webui') return '网页';
    const name = pluginNames.get(id);
    if (!name) return id;
    if ((nameCount.get(name) || 0) > 1) return name + ' (' + id + ')';
    return name;
  }

  // 消息文本兜底：纯图片/聊天记录/卡片的消息没有文字，显示 [消息] 免得一片空白
  function msgText(m) {
    const t = (m.text || '').trim();
    if (t) return t;
    if (m.images && m.images.length) return '[图片]';
    return '[消息]';
  }

  function esc(s) {
    return String(s).replace(/[&<>"]/g, (c) => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;'}[c]));
  }
  function keyOf(t, id) { return t + ':' + id; }
  function fmtTime(ts) {
    if (!ts) return '';
    const d = new Date(ts * 1000);
    const today = new Date();
    if (d.toDateString() === today.toDateString()) return d.toTimeString().slice(0, 5);
    return (d.getMonth() + 1) + '/' + d.getDate();
  }

  function buildList() {
    const lastBy = new Map();
    for (const m of allMsgs()) lastBy.set(keyOf(m.chat_type, m.chat_id), m);

    const q = (search.value || '').trim().toLowerCase();
    let items = contacts.map((c) => {
      const last = lastBy.get(keyOf(c.type, c.id));
      return { type: c.type, id: c.id, name: c.name, last: last ? last.text : '', ts: last ? last.ts : 0 };
    });
    items.sort((a, b) => (b.ts || 0) - (a.ts || 0));

    // 默认只显示聊过的会话；勾了「显示全部」或正在搜索时才展开全部
    if (!showAllBox.checked && !q) {
      const talked = items.filter((c) => c.ts > 0);
      if (talked.length) items = talked;
    }
    if (q) items = items.filter((c) => (c.name || '').toLowerCase().includes(q) || String(c.id).includes(q));

    if (!items.length) { list.innerHTML = '<div class="muted" style="padding:14px">没有会话</div>'; return; }

    let html = '';
    for (const c of items) {
      const active = filter && filter.type === c.type && filter.id === c.id ? ' active' : '';
      const badge = c.type === 'group' ? '群' : '好友';
      html += '<div class="cw-item' + active + '" data-type="' + c.type + '" data-id="' + c.id + '">' +
        '<div class="cw-item-top"><span class="cw-item-name">' + esc(c.name || c.id) + '</span>' +
        '<span class="cw-item-time">' + esc(fmtTime(c.ts)) + '</span></div>' +
        '<div class="cw-item-last">' + (c.last ? esc(c.last) : '<span class="muted">' + badge + ' · ' + c.id + '</span>') + '</div>' +
        '</div>';
    }
    list.innerHTML = html;

    // 虚拟群激活态
    const vEl = document.getElementById('cw-virtual');
    if (vEl) vEl.classList.toggle('active', filter && filter.type === 'virtual');

    list.querySelectorAll('.cw-item').forEach((el) => {
      el.addEventListener('click', () => {
        filter = { type: el.dataset.type, id: Number(el.dataset.id) };
        buildList();
        repaint();
      });
    });
  }

  function repaint() {
    if (!filter) return;
    const idBar = document.getElementById('cw-identity-bar');
    if (idBar) idBar.style.display = filter.type === 'virtual' ? 'block' : 'none';
    if (filter.type === 'virtual') { renderVirtual(); return; }
    const arr = allMsgs().filter((m) => m.chat_type === filter.type && m.chat_id === filter.id);
    if (!arr.length) { msgs.innerHTML = '<div class="muted" style="padding:20px">这个会话还没有消息。</div>'; return; }
    let html = '';
    for (const m of arr) {
      const mine = m.direction === 'out';
      let who = mine ? (m.plugin ? '机器人·' + pluginLabel(m.plugin) : '机器人') : (m.nickname || m.user_id);
      if (!mine && m.plugin) who += ' → ' + pluginLabel(m.plugin);
      let bubbles = esc(msgText(m));
      if (m.images && m.images.length) {
        bubbles += m.images.map((u) =>
          '<img class="cw-img" src="/api/chat/image?url=' + encodeURIComponent(u) + '" loading="lazy"' +
          ' onclick="window.open(this.src)" onerror="this.replaceWith(document.createTextNode(\'[图片加载失败]\'))">'
        ).join('');
      }
      html += '<div class="cw-row ' + (mine ? 'out' : 'in') + '">' +
        '<div class="cw-meta">' + esc(who) + ' · ' + esc(new Date(m.ts * 1000).toLocaleTimeString()) + '</div>' +
        '<div class="cw-bubble">' + bubbles + '</div></div>';
    }
    msgs.innerHTML = html;
    msgs.scrollTop = msgs.scrollHeight;
  }

  async function loadContacts() {
    try {
      const r = await fetch('/api/chat/contacts');
      const data = await r.json();
      contacts = data.contacts || [];
      buildList();
    } catch (e) {}
  }

  async function pollOnce() {
    try {
      const r = await fetch('/api/chat/messages?after=' + after);
      const data = await r.json();
      const arr = data.messages || [];
      if (!arr.length) return;
      let changed = false;
      for (const m of arr) {
        const old = byId.get(m.id);
        if (!old) {
          byId.set(m.id, m);
          changed = true;
        } else if (old.plugin !== m.plugin) {
          // 已经渲染过的消息，插件标注后来补上了 —— 也要刷新
          byId.set(m.id, m);
          changed = true;
        }
      }
      after = arr[arr.length - 1].id;
      if (!changed) return;

      if (!filter) {
        const last = arr[arr.length - 1];
        filter = { type: last.chat_type, id: last.chat_id };
      }
      buildList();
      repaint();
    } catch (e) {}
  }

  function poll() {
    pollOnce();
    setTimeout(poll, 1000);
  }

  search.addEventListener('input', buildList);
  showAllBox.addEventListener('change', buildList);

  form.addEventListener('submit', async (ev) => {
    ev.preventDefault();
    if (!filter) { alert('左边先选一个会话'); return; }
    const content = text.value.trim();
    if (!content) return;
    text.value = '';

    // 虚拟群：走注入，插件回复只显示在这里，不发 QQ
    if (filter.type === 'virtual') {
      await sendToVirtual(content);
      return;
    }

    const body = new URLSearchParams({ chat_type: filter.type, chat_id: filter.id, text: content });
    try {
      const r = await fetch('/api/chat/send', { method: 'POST', body });
      const data = await r.json();
      if (!data.ok) { alert('发送失败: ' + (data.error || '未知错误')); return; }
      setTimeout(pollOnce, 120);
    } catch (e) { alert('发送失败: ' + e.message); }
  });

  // ---- 虚拟群：本地模拟一个群，走框架的真实分发链路 ----
  async function sendToVirtual(content) {
    virtualMsgs.push({ dir: 'in', text: content, ts: Math.floor(Date.now() / 1000) });
    renderVirtual();

    const idSel = document.getElementById('cw-identity');
    const identity = idSel ? idSel.value : 'owner';
    const body = new URLSearchParams({
      text: content,
      chat_type: 'group',
      chat_id: '0',
      identity,
      nickname: '你（虚拟群员）',
    });
    try {
      const r = await fetch('/api/chat/inject', { method: 'POST', body });
      const data = await r.json();
      if (!data.ok) {
        virtualMsgs.push({ dir: 'out', text: '注入失败：' + (data.error || '未知错误'), ts: Math.floor(Date.now() / 1000), plugin: 'webui' });
        renderVirtual();
        return;
      }
      const replies = data.replies || [];
      if (!replies.length && !data.handled) {
        virtualMsgs.push({
          dir: 'out',
          text: '（没有插件认领这条消息）',
          ts: Math.floor(Date.now() / 1000),
          plugin: '',
          muted: true,
        });
      }
      for (const rep of replies) {
        const segs = rep.segments || [];
        const imgs = segs
          .filter((s) => s.type === 'image')
          .map((s) => (s.data && (s.data.url || s.data.file)) || '')
          .filter(Boolean);
        virtualMsgs.push({
          dir: 'out',
          text: segText(segs),
          images: imgs,
          ts: Math.floor(Date.now() / 1000),
          plugin: data.plugin || '',
        });
      }
      renderVirtual(data);
    } catch (e) {
      virtualMsgs.push({ dir: 'out', text: '注入失败：' + e.message, ts: Math.floor(Date.now() / 1000), plugin: 'webui' });
      renderVirtual();
    }
  }

  function segText(segs) {
    if (!segs) return '';
    return segs.map((s) => {
      if (s.type === 'text') return (s.data && s.data.text) || '';
      if (s.type === 'image') return '';
      if (s.type === 'at') return '@' + ((s.data && s.data.qq) || '');
      if (s.type === 'face') return '[表情]';
      if (s.type === 'record') return '[语音]';
      return '[' + s.type + ']';
    }).join('');
  }

  function renderVirtual(meta) {
    if (!filter || filter.type !== 'virtual') return;
    if (!virtualMsgs.length) {
      msgs.innerHTML = '<div class="muted" style="padding:20px">在下面说话，插件的回复会出现在这里。<br>这里只是个模拟群，消息不会发到真实 QQ。</div>';
      return;
    }
    let html = '';
    let lastTs = 0;
    for (const m of virtualMsgs) {
      if (m.ts !== lastTs) {
        html += '<div style="text-align:center;font-size:11px;color:var(--muted);margin:10px 0">' + fmtTime(m.ts) + '</div>';
        lastTs = m.ts;
      }
      const who = m.dir === 'in' ? '你' : (m.plugin ? pluginLabel(m.plugin) : '机器人');
      const muted = m.muted ? ' style="opacity:.6;font-style:italic"' : '';
      html += '<div class="cw-row ' + (m.dir === 'in' ? 'out' : '') + '">' +
        '<div class="cw-meta">' + esc(who) + '</div>' +
        '<div class="cw-bubble"' + muted + '>' + esc(msgText(m)) +
        ((m.images || []).map((u) => '<img class="cw-img" src="/api/chat/image?url=' + encodeURIComponent(u) + '" loading="lazy"' +
          ' onclick="window.open(this.src)" onerror="this.replaceWith(document.createTextNode(\'[图片加载失败]\'))">').join('')) +
        '</div></div>';
    }
    if (meta && meta.elapsed_ms !== undefined) {
      html += '<div style="text-align:center;font-size:11px;color:var(--muted);margin-top:8px">' +
        '分发耗时 ' + meta.elapsed_ms + 'ms' +
        (meta.plugin ? ' · ' + esc(pluginLabel(meta.plugin)) : '') +
        '</div>';
    }
    // 单次消息追踪：一条消息经过了哪些插件、卡在哪
    if (meta && meta.trace && meta.trace.stages) {
      const tr = meta.trace;
      let rows = '';
      for (const st of tr.stages) {
        const mark = st.ok ? '·' : '✗';
        const who = st.plugin ? '「' + esc(pluginLabel(st.plugin)) + '」' : '';
        const why = st.reason ? ' — ' + esc(st.reason) : '';
        const colour = st.ok ? 'var(--muted)' : 'var(--bad)';
        rows += '<div style="color:' + colour + '">' + mark + ' ' + esc(st.name) + who + why + '</div>';
      }
      html += '<details style="margin-top:10px;font-size:11px;color:var(--muted)">' +
        '<summary style="cursor:pointer;user-select:none">追踪 ' + esc(tr.id) + ' · ' + esc(tr.verdict) +
        ' · ' + tr.total_ms + 'ms</summary>' +
        '<div style="margin-top:6px;padding-left:8px;line-height:1.7">' + rows + '</div></details>';
    }
    msgs.innerHTML = html;
    msgs.scrollTop = msgs.scrollHeight;
  }

  // 虚拟群那一行在 #cw-list 外面，得单独绑点击
  const virtualEl = document.getElementById('cw-virtual');
  if (virtualEl) {
    virtualEl.addEventListener('click', () => {
      filter = { type: 'virtual', id: 0 };
      buildList();
      repaint();
      text.focus();
    });
  }

  loadPlugins();
  loadContacts();
  poll();
})();
</script>`)

	s.render(w, "聊天", "/chat", b.String())
}

// apiChatRecent 返回最近有消息的会话，给侧栏做"最近会话"分组。
func (s *Server) apiChatRecent(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	writeJSON(w, http.StatusOK, map[string]any{
		"recent": s.Deps.Chat.Recent(limit),
	})
}

// apiChatImage 代理加载聊天里的图片。
// 协议端给的图片地址通常是内网的，浏览器直接访问不到，所以由服务端去拉。
func (s *Server) apiChatImage(w http.ResponseWriter, r *http.Request) {
	raw := r.URL.Query().Get("url")
	if raw == "" {
		http.Error(w, "缺少 url", http.StatusBadRequest)
		return
	}
	// 插件经常用 base64:// 发图（画布/运势），预览要能解出来
	if strings.HasPrefix(raw, "base64://") {
		data := strings.TrimPrefix(raw, "base64://")
		if i := strings.Index(data, ","); i >= 0 && strings.Contains(data[:i], "base64") {
			data = data[i+1:] // data:image/png;base64,xxx 形式
		}
		b, err := base64.StdEncoding.DecodeString(data)
		if err != nil {
			http.Error(w, "base64 解码失败: "+err.Error(), http.StatusBadRequest)
			return
		}
		ct := http.DetectContentType(b)
		if !strings.HasPrefix(ct, "image/") {
			http.Error(w, "只支持图片内容", http.StatusUnsupportedMediaType)
			return
		}
		w.Header().Set("Content-Type", ct)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "private, max-age=3600")
		_, _ = w.Write(b)
		return
	}

	// 只允许 http/https，避免 file:// 之类
	if !strings.HasPrefix(raw, "http://") && !strings.HasPrefix(raw, "https://") {
		http.Error(w, "只支持 http/https/base64", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()

	b, ct, err := fetchImage(ctx, raw)
	if err != nil {
		http.Error(w, "拉取失败: "+err.Error(), http.StatusBadGateway)
		return
	}
	// 固定为嗅探出的图片类型；禁止浏览器把它当 HTML/脚本执行
	w.Header().Set("Content-Type", ct)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", "inline")
	w.Header().Set("Cache-Control", "private, max-age=3600")
	_, _ = w.Write(b)
}

// apiChatMessages 增量拉取。
func (s *Server) apiChatMessages(w http.ResponseWriter, r *http.Request) {
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	writeJSON(w, http.StatusOK, map[string]any{
		"messages": s.Deps.Chat.Since(after, 200),
	})
}

// apiChatSend 以机器人身份发一条文本。
func (s *Server) apiChatSend(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	chatType := r.FormValue("chat_type")
	chatID, err := strconv.ParseInt(r.FormValue("chat_id"), 10, 64)
	if err != nil {
		http.Error(w, "chat_id 必须是数字", http.StatusBadRequest)
		return
	}
	text := strings.TrimSpace(r.FormValue("text"))
	if (chatType != "group" && chatType != "private") || chatID == 0 || text == "" {
		http.Error(w, "需要 chat_type / chat_id / text", http.StatusBadRequest)
		return
	}
	if len(s.Deps.Config.Accounts) == 0 {
		http.Error(w, "没有配置账号", http.StatusInternalServerError)
		return
	}
	account := s.Deps.Config.Accounts[0].ID

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	id, err := s.Deps.Queue.SendText(ctx, account, chatType, chatID, render.ToPlain(text), 0)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	s.Deps.Chat.Add(chatbuf.Message{
		TS: time.Now().Unix(), Direction: "out", Account: account,
		ChatType: chatType, ChatID: chatID, Plugin: "webui", Text: text,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message_id": id})
}

// apiChatContacts 拉取所有群和好友，给聊天页做会话列表。
func (s *Server) apiChatContacts(w http.ResponseWriter, r *http.Request) {
	if len(s.Deps.Config.Accounts) == 0 {
		http.Error(w, "没有配置账号", http.StatusInternalServerError)
		return
	}
	account := s.Deps.Config.Accounts[0].ID
	cli := s.Deps.Clients[account]
	if cli == nil {
		writeJSON(w, http.StatusOK, map[string]any{"contacts": []any{}, "error": "账号未连接"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()

	type contact struct {
		Type string `json:"type"`
		ID   int64  `json:"id"`
		Name string `json:"name"`
	}
	out := make([]contact, 0, 64)

	var groups []struct {
		GroupID   int64  `json:"group_id"`
		GroupName string `json:"group_name"`
	}
	if err := cli.CallInto(ctx, "get_group_list", map[string]any{}, &groups); err == nil {
		for _, g := range groups {
			out = append(out, contact{Type: "group", ID: g.GroupID, Name: g.GroupName})
		}
	}

	var friends []struct {
		UserID   int64  `json:"user_id"`
		Nickname string `json:"nickname"`
		Remark   string `json:"remark"`
	}
	if err := cli.CallInto(ctx, "get_friend_list", map[string]any{}, &friends); err == nil {
		for _, f := range friends {
			name := f.Remark
			if name == "" {
				name = f.Nickname
			}
			out = append(out, contact{Type: "private", ID: f.UserID, Name: name})
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{"contacts": out})
}
