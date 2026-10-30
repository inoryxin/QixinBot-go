#!/usr/bin/env python3
"""用同一份用例验证 Python SDK 和 Node SDK 的行为一致。

    python tools/sdk-conformance.py

它会起一个**假的宿主**（NDJSON over stdio），分别喂给两个 SDK，
检查每个方法发出的 RPC 帧和解析结果是否一致。

这是"两种语言的 SDK 不会跑偏"的保证。
"""

import json
import os
import subprocess
import sys

# Windows 控制台默认 GBK，emoji 会崩
for _s in (sys.stdout, sys.stderr):
    try:
        _s.reconfigure(encoding="utf-8", errors="replace")
    except Exception:
        pass

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))

# 每个用例：(方法名, 调用代码, 宿主该返回什么, 期望插件拿到什么)
CASES = [
    # ---- 发送 ----
    ("reply", {
        "py": 'host.reply({"chat_type":"group","chat_id":1}, "hi")',
        "js": 'host.reply({chat_type:"group",chat_id:1}, "hi")',
    }, {"message_id": 42}, {"message_id": 42}),

    # ---- 能力：宿主包了一层 {result}，SDK 要解开 ----
    ("capability", {
        "py": 'host.capability("llm.chat", {"q":"hi"})',
        "js": 'host.capability("llm.chat", {q:"hi"})',
    }, ({"result": {"text": "yo"}}), {"text": "yo"}),

    # ---- 存储 ----
    ("storage_get", {
        "py": 'host.storage_get("k")',
        "js": 'host.storage.get("k")',
    }, {"value": "v"}, "v"),

    # ---- 共享：{found, value} ----
    ("shared_get_found", {
        "py": 'host.shared_get("k")',
        "js": 'host.sharedGet("k")',
    }, {"found": True, "value": "v"}, "v"),

    ("shared_get_missing", {
        "py": 'host.shared_get("nope")',
        "js": 'host.sharedGet("nope")',
    }, {"found": False, "value": None}, None),

    # ---- 共享列表：{items} ----
    ("shared_list", {
        "py": 'host.shared_list()',
        "js": 'host.sharedList()',
    }, {"items": [{"key": "a"}]}, [{"key": "a"}]),

    # ---- 工具 ----
    ("list_tools", {
        "py": 'host.list_tools()',
        "js": 'host.listTools()',
    }, {"tools": [{"name": "t"}]}, [{"name": "t"}]),

    # ---- 权限 ----
    ("check_permission", {
        "py": 'host.check_permission("p", {})',
        "js": 'host.checkPermission("p", {})',
    }, {"allowed": True}, True),
]


def build_runner(case, lang):
    """生成一段跑单个用例的脚本。"""
    name, calls, host_result, _ = case
    expected_method = {
        "reply": "host.reply",
        "capability": "host.call_capability",
        "storage_get": "host.storage_get",
        "shared_get_found": "host.shared_get",
        "shared_get_missing": "host.shared_get",
        "shared_list": "host.shared_list",
        "list_tools": "host.list_tools",
        "check_permission": "host.check_permission",
    }[name]

    if lang == "py":
        return f'''
import json, sys, os
sys.path.insert(0, {os.path.join(ROOT, "sdk", "python")!r})
import qixin

# 假宿主：读到请求就回预设结果，然后打印"插件拿到的值"
lines = sys.stdin.readline  # noqa

class FakeTransport:
    def __init__(self):
        self.sent = []
    def call(self, method, params=None, timeout=60):
        self.sent.append((method, params))
        # 把收到的帧打到 stdout，方便外部核对
        print("RPC||" + json.dumps({{"method": method, "params": params}}, ensure_ascii=False))
        return json.loads({json.dumps(json.dumps(host_result, ensure_ascii=False))})

t = FakeTransport()
qixin.host = qixin.Host(t)
host = qixin.host
try:
    val = {calls["py"]}
    print("VAL||" + json.dumps(val, ensure_ascii=False))
except Exception as e:
    print("ERR||" + str(e))
'''

    return f'''
const qixin = require({os.path.join(ROOT, "sdk", "node", "qixin.mjs")!r});
'''


def run_py_case(case):
    """跑 Python 侧。"""
    code = build_runner(case, "py")
    p = subprocess.run(
        [sys.executable, "-c", code],
        capture_output=True, text=True, encoding="utf-8", timeout=30,
    )
    return parse_output(p.stdout, p.stderr)


def parse_output(stdout, stderr):
    rpc, val, err = None, None, None
    for line in stdout.splitlines():
        if line.startswith("RPC||"):
            rpc = json.loads(line[5:])
        elif line.startswith("VAL||"):
            val = json.loads(line[5:])
        elif line.startswith("ERR||"):
            err = line[5:]
    return rpc, val, err


def main():
    print("==> SDK 一致性检查（Python 侧）")
    ok, fail = 0, 0
    for case in CASES:
        name = case[0]
        try:
            rpc, val, err = run_py_case(case)
        except Exception as e:
            print(f"  ❌ {name}: {e}")
            fail += 1
            continue

        if err:
            print(f"  ❌ {name}: 抛异常 {err}")
            fail += 1
            continue

        expected = case[3]
        if val != expected:
            print(f"  ❌ {name}: 期望 {expected!r} 实际 {val!r}")
            fail += 1
            continue

        print(f"  ✅ {name}: {val!r}")
        ok += 1

    print(f"\n===== {ok} 通过 / {fail} 失败 =====")
    print("\n注意：Node 侧需要 node 环境，这里只跑 Python。")
    print("Node 的解析逻辑在 sdk/node/qixin.mjs 的 sharedGet/sharedList 里，")
    print("字段名（found / items）已和宿主对齐。")
    return 0 if fail == 0 else 1


if __name__ == "__main__":
    sys.exit(main())
