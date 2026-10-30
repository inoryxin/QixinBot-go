#!/usr/bin/env python3
"""qixin Python SDK —— 和 Node 版对齐。

用法：

    from qixin import serve, host, chat_of

    def initialize(params):
        return {"api_version": "1.0"}

    def handle_message(params):
        s = params["session"]
        host.reply(chat_of(s), "你好")
        return {"handled": True}

    serve(initialize=initialize, handle_message=handle_message)

**两条不可协商的规则**：
  1. stdout 只走协议，任何 print 都会污染通道（日志用 host.log）
  2. 一行一个 JSON

**和 Node 版的对应**：

| Node | Python |
|---|---|
| `host.reply(chat, segs)` | `host.reply(chat, segs)` |
| `host.ask(chat, text, opts)` | `host.ask(chat, text, timeout_sec=60)` |
| `host.background(session, name, fn)` | `host.background(session, name, fn)` |
| `host.capability(name, args)` | 同名 |
| `host.storage.get/set` | `host.storage_get/storage_set` |
| `host.sharedGet/Set/List` | `host.shared_get/shared_set/shared_list` |
| `host.checkPermission` | `host.check_permission` |
| `host.listTools` | `host.list_tools` |
| `host.callOnebot` | `host.call_onebot` |

**线程安全的做法**：读 stdin 在独立线程，`call()` 用 Event 等结果。
这样 handler 里调宿主不会和读循环抢 stdin（旧版会吞掉宿主发来的 ping）。
"""

import json
import os
import sys
import threading
import time
import traceback

# Windows 上默认 GBK，协议必须是 UTF-8
for _stream in (sys.stdin, sys.stdout, sys.stderr):
    try:
        _stream.reconfigure(encoding="utf-8", errors="replace")
    except Exception:
        pass


class _Seq:
    def __init__(self):
        self._n = 0
        self._lock = threading.Lock()

    def next(self):
        with self._lock:
            self._n += 1
            return self._n


def _segs(segments):
    """允许传字符串、单段 dict、段列表。"""
    if isinstance(segments, str):
        return [{"type": "text", "data": {"text": segments}}]
    if isinstance(segments, dict):
        return [segments]
    return list(segments)


class Host:
    """宿主能力。所有方法同步返回。"""

    def __init__(self, transport):
        self._t = transport

    # ---- 发送 ----

    def reply(self, chat, segments, reply_to=0):
        return self._t.call("host.reply", {
            "chat": chat, "segments": _segs(segments), "reply_to": reply_to,
        })

    def send_message(self, chat, segments, reply_to=0):
        return self._t.call("host.send_message", {
            "chat": chat, "segments": _segs(segments), "reply_to": reply_to,
        })

    # ---- 交互 ----

    def ask(self, chat, text, timeout_sec=60, user_id=0):
        """发提示并等用户下一条消息。**超时返回空字符串**，不抛异常。"""
        r = self._t.call("host.ask", {
            "chat": chat,
            "text": text,
            "user_id": user_id or chat.get("user_id", 0),
            "timeout_sec": timeout_sec,
        }, timeout=timeout_sec + 15)
        return (r or {}).get("text", "") or ""

    # ---- 后台任务 ----

    def background(self, session, name, fn):
        """起后台线程跑 fn，**不阻塞** handle_message。

        不活过重启——要跨重启就把进度/上次时间存 host.storage，启动时续跑。
        """
        job_id = "pyjob-%d-%d" % (_Seq_global.next(), int(time.time() * 1000))
        chat = chat_of(session) if session else None
        try:
            self._t.call("host.job_begin", {"id": job_id, "name": name, "chat": chat})
        except Exception:
            pass

        def runner():
            err = ""
            try:
                fn()
            except Exception as e:
                err = str(e)
                self.log("后台任务「%s」失败: %s" % (name, e), level="error")
            try:
                self._t.call("host.job_end", {"id": job_id, "error": err})
            except Exception:
                pass

        threading.Thread(target=runner, name="qixin-job-" + name, daemon=True).start()
        return job_id

    # ---- 能力 ----

    def capability(self, name, args=None):
        r = self._t.call("host.call_capability", {"name": name, "args": args or {}})
        # 宿主包了一层 {result: ...}
        if isinstance(r, dict) and "result" in r:
            return r["result"]
        return r

    def call_tool(self, name, args=None, session=None):
        params = {"name": name, "args": args or {}}
        if session is not None:
            params["session"] = session
        return self._t.call("host.call_tool", params)

    # ---- 存储 ----

    def storage_get(self, key):
        r = self._t.call("host.storage_get", {"key": key})
        return (r or {}).get("value")

    def storage_set(self, key, value):
        self._t.call("host.storage_set", {"key": key, "value": value})
        return True

    def storage_delete(self, key):
        self._t.call("host.storage_delete", {"key": key})
        return True

    def storage_list(self, prefix=""):
        r = self._t.call("host.storage_list", {"prefix": prefix})
        return (r or {}).get("keys") or []

    # ---- 共享数据 ----

    def shared_get(self, key):
        """返回共享值；没有这个 key 返回 None（和 Node 的 sharedGet 一致）。"""
        r = self._t.call("host.shared_get", {"key": key})
        return (r or {}).get("value") if (r or {}).get("found") else None

    def shared_set(self, key, value):
        r = self._t.call("host.shared_set", {"key": key, "value": value})
        return bool((r or {}).get("ok"))

    def shared_list(self):
        # 宿主返回 {"items": [...]}
        r = self._t.call("host.shared_list", {})
        return (r or {}).get("items") or []

    # ---- 其它 ----

    def call_onebot(self, action, params=None):
        return self._t.call("host.call_onebot", {"action": action, "params": params or {}})

    def list_tools(self, session=None):
        params = {}
        if session is not None:
            params["session"] = session
        r = self._t.call("host.list_tools", params)
        return (r or {}).get("tools") or []

    def check_permission(self, plugin, session):
        r = self._t.call("host.check_permission", {"plugin": plugin, "session": session})
        return bool((r or {}).get("allowed"))

    def list_access(self):
        r = self._t.call("host.list_access", {})
        return (r or {}).get("access") or {}

    def log(self, msg, level="info", **fields):
        log(msg, level, **fields)


class Transport:
    """NDJSON over stdio，读线程 + Event。"""

    # 类级写锁：模块级 log() 不经过任何 Transport 实例也要和
    # send() 互斥，否则两个线程的 NDJSON 帧会交错出一行废协议
    _write_lock = threading.Lock()

    def __init__(self, host_obj=None):
        self._pending = {}          # req_id -> (Event, box)
        self._lock = threading.Lock()
        self._seq = _Seq()
        self._handlers = {}
        self._running = True
        # 允许外部传入现成的 Host：serve() 要复用模块级单例，
        # 另建一个的话插件手里的还是没接线的那个
        if host_obj is not None:
            host_obj._t = self
            self.host = host_obj
        else:
            self.host = Host(self)

    # ---- 底层收发 ----

    def send(self, frame):
        line = json.dumps(frame, ensure_ascii=False)
        with Transport._write_lock:
            sys.stdout.write(line + "\n")
            sys.stdout.flush()

    def call(self, method, params=None, timeout=60):
        """发请求等响应。多线程会各自等自己的 id，不会互相吞。"""
        req_id = self._seq.next()
        ev = threading.Event()
        box = {}

        with self._lock:
            self._pending[req_id] = (ev, box)

        self.send({
            "jsonrpc": "2.0", "id": req_id,
            "method": method, "params": params or {},
        })

        if not ev.wait(timeout):
            with self._lock:
                self._pending.pop(req_id, None)
            raise TimeoutError("宿主方法 %s 超时（%ds）" % (method, timeout))

        if box.get("error"):
            e = box["error"]
            raise RuntimeError("[%s] %s" % (e.get("code"), e.get("message")))
        return box.get("result")

    # ---- 读线程 ----

    def _read_loop(self):
        while self._running:
            line = sys.stdin.readline()
            if not line:
                self._running = False
                break
            line = line.strip()
            if not line:
                continue
            try:
                frame = json.loads(line)
            except json.JSONDecodeError:
                log("收到非 JSON 行（stdout 被污染？）: " + line[:100], "warn")
                continue

            # 响应：交给等它的 call
            if "method" not in frame:
                with self._lock:
                    waiter = self._pending.get(frame.get("id"))
                if waiter:
                    ev, box = waiter
                    if "error" in frame:
                        box["error"] = frame["error"]
                    else:
                        box["result"] = frame.get("result")
                    ev.set()
                continue

            # 请求：交给 handler（**在独立线程跑**，
            # 否则 handler 里调宿主会卡住读循环）
            threading.Thread(
                target=self._dispatch, args=(frame,), daemon=True
            ).start()

    def _dispatch(self, frame):
        method = frame.get("method", "")
        req_id = frame.get("id", "")
        params = frame.get("params") or {}

        # plugin.handle_message → handle_message
        name = method.replace("plugin.", "").replace(".", "_")
        fn = self._handlers.get(name) or DEFAULTS.get(name)
        if fn is None:
            if req_id:
                self.send({
                    "jsonrpc": "2.0", "id": req_id,
                    "error": {"code": -32601, "message": "未实现: " + method},
                })
            return

        try:
            result = fn(params)
            if result is None:
                result = {}
        except Exception as e:
            log("%s 失败: %s\n%s" % (method, e, traceback.format_exc()), "error")
            if req_id:
                self.send({
                    "jsonrpc": "2.0", "id": req_id,
                    "error": {"code": -32603, "message": str(e)},
                })
            return

        if req_id:
            self.send({"jsonrpc": "2.0", "id": req_id, "result": result})

    def run(self, handlers):
        self._handlers = handlers
        t = threading.Thread(target=self._read_loop, name="qixin-read", daemon=True)
        t.start()
        t.join()


# ---- 模块级 ----

_Seq_global = _Seq()
# 没实现的方法用这些默认值（与 Node SDK 的 DEFAULTS 对齐）：
# 最小插件只需要写 handle_message；observe/shutdown 等未实现时静默按默认行为走，
# 否则声明了 observe=true 却没实现 observe_message 的插件会每次吃 -32601。
DEFAULTS = {
    "initialize": lambda p: {"api_version": "1.0"},
    "ping": lambda p: {"pong": True},
    "handle_message": lambda p: {"handled": False},
    "handle_event": lambda p: {},
    "observe_message": lambda p: {},
    "shutdown": lambda p: {},
    "list_tools": lambda p: {"tools": []},
    "get_settings": lambda p: {"values": {}},
    "set_settings": lambda p: {"ok": True},
}

host = Host(None)  # serve() 时接到真实 transport（对象不变，_t 被填上）


def log(msg, level="info", **fields):
    """发一条日志给宿主（走协议，不走 stderr 污染）。"""
    frame = {
        "jsonrpc": "2.0", "method": "host.log",
        "params": {"level": level, "message": str(msg), "fields": fields},
    }
    try:
        # 和 Transport.send 共用类级写锁，别把协议行写交叉
        with Transport._write_lock:
            sys.stdout.write(json.dumps(frame, ensure_ascii=False) + "\n")
            sys.stdout.flush()
    except Exception:
        pass


def chat_of(session):
    """从 session 里抠出发送目标。
    带 user_id：host.ask 靠 (chat_type, chat_id, user_id) 登记等待者，
    不带的话等待者永远注册在 user_id=0 上，用户的回复永远对不上号。"""
    return {
        "account": session.get("account", ""),
        "chat_type": session.get("chat_type", "group"),
        "chat_id": session.get("chat_id", 0),
        "user_id": session.get("user_id", 0),
    }


def serve(**handlers):
    """启动：注册 handler，起读线程，阻塞到宿主断开。

    handlers 的 key 是方法名（initialize / handle_message / ...），
    value 是接收 params、返回 result 的函数。
    """
    # 直接把模块级单例接上 transport，**不能**重新赋值模块属性：
    # 文档写的是 `from qixin import host`，插件模块手里攥着的是
    # import 时的那个对象；`global host; host = t.host` 只换了
    # qixin 模块的属性，插件手里的还是 Host(None)，
    # 第一次 host.reply 就 AttributeError。之前 sdk-conformance
    # 自己绕开了这个坑，所以测试一直没暴露
    t = Transport(host)
    t.run(handlers)
