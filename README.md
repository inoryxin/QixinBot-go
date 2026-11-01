<p align="center">
  <img src="branding/logo-256.png" width="120" alt="QixinBot">
</p>

<h1 align="center">QixinBot</h1>

<p align="center">基于 OneBot 11 的机器人框架：<b>宿主用 Go，插件用任意语言</b>（独立子进程，stdio + JSON-RPC）</p>

<p align="center">
  <a href="https://bot.inory.xin">官网</a> ·
  <a href="https://github.com/inoryxin/QixinBot-go/releases">下载</a> ·
  <a href="https://github.com/inoryxin/QixinBot-go/wiki">文档</a> ·
  <a href="LICENSE">MIT License</a>
</p>

---

## 这是什么

一个**插件即进程**的 QQ 机器人框架。宿主（Go 单二进制）负责连接协议端、权限、分发、
控制台；每个插件是一个独立子进程，可以用 Node.js、Python，或任何能读 stdin 写 stdout 的语言写。

插件崩了不会带崩框架，框架挂了插件也一起停——边界清晰，2G 内存的小机器也能跑。

## 特性

**稳定性**

- 插件独立进程 + 退避重启（1/2/4/8/16s，连续 6 次禁用）；稳定运行 10 分钟后计数清零
- 挂死自愈：连续 5 次消息超时自动禁用，不拖慢分发
- OneBot 连接 30s 心跳保活：对端假活秒级检出并重连
- 写管道 5s 超时判死重建，不会"进程活着但全哑"

**内存（cohost 共享宿主，默认开启）**

- 声明 `cohost = true` 的纯 JS 插件共用一个 Node 进程（每插件一个 worker 线程）
- 多池隔离：按池分组、单插件 24h 崩 3 次自动逐出回独占、池级熔断
- 实测 28 插件：独占 1.0GB → 共享 ~430MB

**插件能力**

- 热插拔：`.anzu` 丢进 `plugins/` 自动安装，删掉自动卸载（数据保留）
- 能力（capability）：插件之间互相调用，多提供者按优先级选，失败自动降级
- 工具（tool）：给 AI 插件调用的函数，带参数 schema 校验
- 设置页由 manifest 声明，控制台自动渲染；每次保存轮转 4 份备份
- 签名：ed25519，官方公钥内置，第三方可自带公钥

**运维**

- WebUI 控制台 10 个板块：概览 / 连接 / 权限 / 插件 / 共享池 / 依赖 / 指令 / 聊天 / 审计 / 日志
- 24h 指标趋势（消息 / 插件延迟 / 内存 / 队列深度）
- 配置热重载（日志级别、发送限频、群白名单）
- 日志按 8MB 轮转；`/readyz` 就绪探针
- 消息全链路 trace：一条消息卡在哪一步一眼可见

## 快速开始

### 1. 下载

到 [Releases](https://github.com/inoryxin/QixinBot-go/releases) 页面按平台下载：

| 文件 | 说明 |
|---|---|
| `1-QixinBot-win-v1.0.0.zip` | Windows 框架包（qixinbot.exe + SDK + 配置模板 + 启动脚本） |
| `2-QixinBot-linux-v1.0.0.zip` | Linux 框架包（同上） |
| `3-6-runtime-*.anzu` | 运行时包（插件用 Node/Python 时必装，自带 canvas / Pillow 等） |
| `7-8-QixinBot-devtools-*.zip` | 插件开发工具（qixinpack + mock-onebot），**只写插件才需要** |

解压到任意目录，例如 `C:\qixinbot` 或 `/opt/qixinbot`。**不用装 Go、不用装 Node/Python；
框架和插件解释器都由上面的包提供。**

### 2. 改配置

打开根目录的 `config.toml`，至少改这两处：

```toml
[[accounts]]
id = "main"
ws_url = "ws://127.0.0.1:3001"    # 你的 OneBot 协议端地址
access_token = ""                 # 协议端配了令牌就填上，没配留空

[web]
addr = "0.0.0.0:7801"             # 控制台监听地址
password = "改个密码"              # 留空则每次启动生成临时密码，见 data/webui-password.txt
```

协议端推荐 [SnowLuma](https://github.com/SnowLuma/SnowLuma)：在它里面开 OneBot 11
WebSocket 服务端（默认 3001），登录 QQ 后框架会自动连上并重连。

### 3. 启动

**Windows**（双击或命令行）：

```powershell
start.bat
```

**Linux**（`start.sh` 会自动补执行位）：

```bash
sh start.sh
```

日志里出现 `已连接上游`、`登录账号 self_id=...` 就是连上了。

### 4. 打开控制台

浏览器访问 `http://<这台机器>:7801`，用 `config.toml` 里的账号密码登录。
没设密码时，临时密码在 `data/webui-password.txt`。

控制台 10 个板块：概览 / 连接 / 权限 / 插件 / 共享池 / 依赖 / 指令 / 聊天 / 审计 / 日志。

### 5. 装运行时和插件

**运行时**（插件用 Node/Python 时必须）：控制台「依赖」页上传 `runtime-*.anzu`，
本机用不了的平台包会被拒绝。

**插件**：把 `.anzu` 丢进 `plugins/` 目录，2 秒内自动装好（默认未启用），
去「插件」页点「启用」；也可以在「插件」页直接上传。

**插件从哪来**：官方插件仓库
[QixinBot-go-plugins](https://github.com/inoryxin/QixinBot-go-plugins)（源码 + 签名 `.anzu` 包，
各插件的说明在仓库里各自的目录）；也可以先用自己的或第三方的 `.anzu`
（`qixinpack pack` 打出来的就是，签名包开箱可装）。第三方插件想展示在插件商店：
发[收录申请 Issue](https://github.com/inoryxin/QixinBot-go-plugins/issues/new?template=plugin-submission.yml)。

### 6. 常用的几个开关

```toml
[plugin]
hot_reload = true                 # 投放区热插拔（丢包自动装）
allow_unsigned = true             # 开发时允许装未签名包（生产建议关掉）
cohost_enabled = false            # 关掉共享池（默认开，省内存）

[log]
level = "debug"                   # 排查时开 debug
gate_debug = true                 # 打印被门禁拦下的消息及原因
```

日志级别、发送限频、群白名单是热字段，改完 5 秒生效；账号和协议端地址改完要重启。

## 写一个插件

```bash
# 开发工具包：releases 页的 devtools zip（qixinpack + mock-onebot）
chmod +x bin/* && cp qixinpack mock-onebot <框架包>/bin/
cd <框架包>/bin

./qixinpack new my-plugin -name 我的插件
./qixinpack dev my-plugin -watch     # 假协议端 + 框架，保存即重载
./qixinpack test my-plugin           # 按 manifest 自动跑命令
./qixinpack pack my-plugin -out plugins/my-plugin.anzu
```

完整手册见 [Wiki：插件开发快速开始](https://github.com/inoryxin/QixinBot-go/wiki/%E6%8F%92%E4%BB%B6%E5%BC%80%E5%8F%91-%E5%BF%AB%E9%80%9F%E5%BC%80%E5%A7%8B)。

## 目录结构

```
cmd/bot/           宿主入口（装配、消息循环、关闭）
internal/
  onebot/          OneBot 11 WS 客户端（心跳、重连、事件解析）
  plugin/          插件管理器：生命周期、分发、cohost、安装器、权限判定
  session/         会话与结构化命令
  webui/           控制台（页面、API、主题、探针）
  send/ storage/ shared/ perm/ registry/ audit/ ...
sdk/node|python/   插件 SDK（打包时注入到插件里）
tools/qixinpack    .anzu 打包 / 开发环境 / 端到端验收
tools/mock-onebot  假协议端（开发测试用）
deploy/            start 脚本、systemd 单元、配置示例
branding/          标识与各尺寸图标
```

## 相关

- **协议端**：推荐 [SnowLuma](https://github.com/SnowLuma/SnowLuma)（OneBot 11 实现）

## 许可证

[MIT](LICENSE)
