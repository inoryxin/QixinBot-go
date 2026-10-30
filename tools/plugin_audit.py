"""按 PLUGIN.md §14 自测清单检查现有插件。"""
import pathlib
import re
import json

ROOT = pathlib.Path("dist")

results = []

for d in sorted(p for p in ROOT.iterdir() if p.is_dir() and not p.name.endswith("-src")):
    man = d / "manifest.toml"
    app = d / "app" / "index.js"
    if not man.exists():
        continue

    txt = man.read_text(encoding="utf-8")
    js = app.read_text(encoding="utf-8") if app.exists() else ""
    # 去掉行注释再检查，避免把说明文字当成代码
    js_code = re.sub(r"^\s*//.*$", "", js, flags=re.M)
    panel = d / "web" / "panel.html"
    html = panel.read_text(encoding="utf-8") if panel.exists() else ""

    issues = []

    # 1. usage 带前缀
    for m in re.finditer(r'usage\s*=\s*"([^"]*)"', txt):
        if m.group(1).startswith(("/", "!", "#")):
            issues.append(f"usage 带前缀: {m.group(1)}")

    # 2. require_prefix=false 但没配 allow_users
    if re.search(r"require_prefix\s*=\s*false", txt):
        has_allow = re.search(r"allow_users\s*=\s*\[", txt)
        # 设置项里有没有 allowUsers
        has_setting = re.search(r'key\s*=\s*"allowUsers"', txt)
        if not has_allow and not has_setting:
            # 纯查询/纯聊天类可以豁免，有副作用的才标出来
            harmless = not re.search(r"sendMessage|callOnebot|fetch\(|http", js_code) or "ai.chat" in txt or "ai.persona" in txt
            if not harmless:
                issues.append("require_prefix=false 且无 allow_users（有副作用就危险）")

    # 3. 自己 setInterval
    if "setInterval(" in js_code:
        issues.append("用了 setInterval（应该用 [[schedule]]）")

    # 4. 自己读文件存状态
    if re.search(r"readFileSync|writeFileSync", js):
        issues.append("自己读写文件（应该用 host.storage）")

    # 5. 推送传 account
    for m in re.finditer(r"sendMessage\s*\(\s*\{[^}]*\}", js_code, re.S):
        if "account" in m.group(0):
            issues.append("sendMessage 传了 account（应该交给框架）")
            break

    # 6. 原生模块没写 platform
    if (d / "app" / "node_modules").exists() and "platform" not in txt:
        issues.append("带 node_modules 但没写 platform")

    # 7. console.log
    if re.search(r"\bconsole\.(log|error|warn)\(", js):
        issues.append("用了 console.*（会破坏协议）")

    # 8. 界面英文（简单检查：panel.html 里连续英文单词占比）
    if html:
        # 找中文，如果整页没有中文就是英文页
        if not re.search(r"[\u4e00-\u9fff]", html):
            issues.append("面板没有中文")

    # 9. 工具 description
    for m in re.finditer(r'\[\[tools\]\]\s*\nname\s*=\s*"([^"]*)"\s*\ndescription\s*=\s*"([^"]*)"', txt):
        if len(m.group(2)) < 10:
            issues.append(f"工具 {m.group(1)} 的 description 太短")

    results.append({
        "id": d.name,
        "issues": issues,
        "commands": len(re.findall(r"\[\[commands\]\]", txt)),
        "tools": len(re.findall(r"\[\[tools\]\]", txt)),
        "schedule": len(re.findall(r"\[\[schedule\]\]", txt)),
        "provides": len(re.findall(r"@1", txt.split("[[")[0])),
    })

# 输出
print(f"{'插件':<16} {'命令':>4} {'工具':>4} {'定时':>4} {'能力':>4}  问题")
print("-" * 90)
for r in results:
    flag = "OK" if not r["issues"] else f"{len(r['issues'])} 项"
    print(f"{r['id']:<16} {r['commands']:>4} {r['tools']:>4} {r['schedule']:>4} {r['provides']:>4}  {flag}")
    for i in r["issues"]:
        print(f"{'':<18}  - {i}")

print()
total = sum(len(r["issues"]) for r in results)
print(f"合计 {len(results)} 个插件，{total} 个问题")
