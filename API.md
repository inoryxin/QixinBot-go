# 接口说明

<!-- TOC-BEGIN -->
**目录**

- [0. 兼容性承诺](#0-兼容性承诺)
- [1. 约定](#1-约定)
- [2. 宿主 → 插件](#2-宿主--插件)
- [3. 插件 → 宿主](#3-插件--宿主)
- [4. 数据结构](#4-数据结构)
- [5. HTTP 接口](#5-http-接口)
- [6. 错误码](#6-错误码)
- [7. 权限检查点](#7-权限检查点)
- [附：[cooldown] 配置](#附cooldown-配置)
- [附：[timeouts] 配置](#附timeouts-配置)
- [附：[[capability]] 的 max_concurrent](#附capability-的-max_concurrent)
- [附：load_order 加载顺序](#附load_order-加载顺序)

<!-- TOC-END -->




- 版本：1.0
- 日期：2026-10-24
- 关联：[PROTOCOL.md](./PROTOCOL.md)（传输细节）、[PLUGIN.md](./PLUGIN.md)（插件规范）

本文列出**当前实现**的全部接口。标 ⏳ 的是协议里留了但宿主还没接。

---

## 0. 兼容性承诺

**这一节是给插件作者的合同**：什么能依赖，什么随时会变。

### 版本号怎么说

`manifest.toml` 的 `apiVersion = "1.x"`：

| 字段 | 含义 |
|---|---|
| 主版本（`1`） | **不兼容**时才加。框架加载时会拒绝主版本不匹配的插件 |
| 次版本（`x`） | 框架**只增不改**。旧插件在新框架上必须还能跑 |

**你写 `1.x` 或 `1.0` 都行**——框架只比较主版本。

```toml
apiVersion = "1.x"   # 推荐：接受 1.0 ~ 1.99 的所有框架
apiVersion = "2.x"   # 主版本 2 的框架才会加载
```

### 哪些是稳定 API（可以依赖）

| 类别 | 稳定性 | 说明 |
|---|---|---|
| `plugin.initialize` / `handle_message` / `handle_event` / `call_tool` / `get_settings` / `set_settings` / `shutdown` | **稳定** | 签名不会变，只会加可选参数 |
| `host.reply` / `send_message` / `call_capability` / `call_tool` / `storage_*` / `shared_*` / `log` | **稳定** | 同上 |
| manifest 的 `id` / `name` / `version` / `apiVersion` / `exec` / `[[commands]]` / `[[settings]]` / `[[tools]]` / `[[requires]]` / `[runtime]` / `[access]` | **稳定** | 只会加新字段，不改语义 |
| 消息段格式（`{type, data}`） | **稳定** | OneBot 11 的标准，不会动 |
| `.anzu` 包结构 | **稳定** | `manifest.toml` + `app/` + `deps/` + `assets/` + `web/` |
| HTTP API（`/api/*`） | **次要** | 内部接口，可能随版本变；插件不该依赖 |

### 哪些会变（别依赖）

| 东西 | 风险 |
|---|---|
| 日志格式 | 随时改进，插件别解析自己的 `host.log` 输出 |
| `POST /api/chat/inject` 的 `image` 参数 | 传 http(s) 图片地址，可以只发图（text 留空） |
| `POST /api/runtime` (kind=node\|python, path) | 指定解释器绝对路径；留空恢复自动探测；写 data/runtime.json |
| `plugin.observe_message` | 声明 `observe = true` 后，每条消息都会抄一份；返回值忽略 |
| `GET /api/trusted-keys` | 返回 `{keys:[{key,fingerprint,source}], allow_unsigned}`；`source` = `official` / `config` / `added` |
| `POST /api/trusted-keys` | 表单 `key=`，立刻生效（落盘 `data/trusted-keys.json`） |
| `POST /api/trusted-keys/delete` | 表单 `key=`，只能删热加的（config 里的会报错提示改文件） |
| 插件上传 | `POST /api/plugins/upload` **走签名校验**（和投放区安装同一套逻辑） |
| `host.log` 的级别 | 支持 `debug/info/warn/error`，映射到框架日志级别；`fields` 为空不输出 |
| 内部错误消息文本 | 只保证错误码稳定，`message` 可能改措辞 |
| `install.json` 的字段 | 框架内部用的，插件读它随时可能坏 |
| WebUI 的 HTML 结构 | 会被改版，别写脚本抓页面 |
| 框架内部的 Go 包路径 | 可以不打招呼就重构 |

### 框架承诺的事

**次版本升级时**：

1. ✅ 旧插件**不用改**就能加载
2. ✅ 已有的 `host.*` 方法**不会删**
3. ✅ 已有的 manifest 字段**语义不变**
4. ✅ 消息段格式**不改**
5. ⚠️ 可能**新增**字段/方法（你要是不认，忽略就行）

**真的需要破坏时**：

- 升主版本（`1.x` → `2.x`）
- 在 `REQUIREMENTS.md` 里写迁移说明
- 旧插件在 2.x 上会**明确报错**（"这个插件要求 apiVersion 1.x，当前框架是 2.x"），而不是静默坏掉

### 插件该怎么做兼容

```toml
apiVersion = "1.x"     # 接受整个 1.x 系列
```

**别写死小版本**：

```toml
apiVersion = "1.3"     # ❌ 框架升到 1.4 就可能不认
```

**用能力前先检查**（能力可能被卸载）：

```js
const r = await host.capability('llm.chat', { messages })
if (!r?.ok) {
  // 能力不可用时的降级路径
  await host.reply(chat, '当前没配 LLM，先回你一句：收到')
  return { handled: true }
}
```

**能力调用默认不自动降级**：能力可能有副作用（发消息、扣款、控设备），
失败直接返回错误，不会换个提供者再执行一遍。
manifest 里声明 `retry = "always"` 的能力，超时/不可用/内部错误时
才会按优先级试下一个提供者；参数错和权限错永远不重试（换谁都一样）。
提供者没运行的会先拉起，失败就跳到下一个。

**`[[requires]]` 声明依赖**（框架加载时会检查，缺了给你 `missing_dependency` 而不是崩溃）：

```toml
[[requires]]
kind = "plugin"
value = "llm"
```

### 我的建议

**插件作者**：

1. `apiVersion` 写 `"1.x"`
2. 用 `[[requires]]` 声明硬依赖
3. 调能力时检查 `r.ok`
4. 别解析日志、别读 `install.json`

**框架维护者（我）**：

1. 任何 `host.*` 的签名改动都算**破坏性变更**
2. 删字段 = 升主版本
3. 加字段 = 次版本，但**要有默认值**，老插件不写也能跑

---

## 1. 约定

| 项 | 规则 |
|---|---|
| 编码 | UTF-8，一行一个 JSON（NDJSON） |
| 行上限 | 16 MiB；超长整行丢弃并记 warn（**不断连**） |
| id | 宿主发起的带 `h-`，插件发起的带 `p-` |
| 超时 | 心跳 3s / 工具 8s / 事件 10s / 消息 30s |
| stdout | **只能走协议**，日志一律 stderr |

---

## 2. 宿主 → 插件

### 2.1 `plugin.initialize`

握手。插件必须返回 `api_version`，宿主校验主版本。

```json
// 参数
{
  "plugin_id": "weather",
  "api_version": "1.0",
  "host_version": "0.1.0",
  "data_dir": "C:\\qqbot\\data\\plugins\\weather\\data",
  "settings": { "unit": "celsius" }
}

// 返回
{ "api_version": "1.0" }
```

`settings` 是**已合并默认值**的配置，插件第一次拿配置就读这里。

### 2.2 `plugin.handle_message`

```json
// 参数
{ "session": { ... } }

// 返回
{ "handled": true }
```

- `true`：消息归我，停止分发给后续插件
- `false`：不接管，继续问下一个
- 抛错：宿主记日志并继续下一个

回复**不通过返回值**，用 `host.reply`（见 3.1）。

### 2.3 `plugin.handle_event`

通知 / 请求 / 元事件。

```json
// 参数
{
  "event": {
    "account": "main",
    "post_type": "notice",
    "notice_type": "group_increase",
    "sub_type": "approve",
    "self_id": 1102679600,
    "user_id": 2849446053,
    "group_id": 123456,
    "operator_id": 0,
    "time": 1761200000,
    "raw": { }
  }
}

// 返回
{}
```

返回值被忽略，插件可以在这里做副作用（发欢迎语等）。

### 2.4 `plugin.list_tools`

⏳ 宿主目前从 manifest 读工具，不调这个方法。保留给"运行时动态注册工具"。

```json
// 返回
{ "tools": [ { "name": "weather_query", "description": "...", "parameters": {} } ] }
```

### 2.5 `plugin.call_tool`

```json
// 参数
{
  "name": "weather_query",
  "args": { "city": "北京" },
  "session": { ... }        // 可能为 null（来自 WebUI 或插件互调）
}

// 返回（三选一）
{ "ok": true,  "data": { "text": "北京 22°C 晴" } }
{ "ok": false, "error": "城市不存在", "retryable": false }
{ "ok": false, "error": "上游超时", "retryable": true }
```

工具超时 8 秒。**不要抛异常**，失败也返回 `{ok:false}`。

### 2.6 `plugin.get_settings` ⏳ / `plugin.set_settings`

`get_settings` 宿主目前不调用（配置由宿主从 `settings.json` 读）。保留给"插件自报配置"的场景。

```json
// get_settings 返回（未使用）
{ "values": { "unit": "celsius" } }

// set_settings 参数（宿主主动推）
{ "values": { "unit": "fahrenheit" } }
// 返回
{ "ok": true }
```

宿主保存设置后会推 `set_settings`，插件更新内存里的值即可。

### 2.7 `plugin.ping` / `plugin.shutdown`

```json
// ping
{ }  →  { "pong": true }

// shutdown
{ "reason": "host_shutdown" }  →  { }
```

`shutdown` 后宿主关 stdin，10 秒不退则 SIGTERM。

---

## 3. 插件 → 宿主

### 3.1 `host.reply`

回复当前会话。**是否引用由插件决定**：传 `reply_to` 就引用。

```json
{
  "chat": { "account": "main", "chat_type": "group", "chat_id": 123456 },
  "segments": [ { "type": "text", "data": { "text": "收到" } } ],
  "reply_to": 0
}

// 返回
{ "message_id": 10086 }
```

`segments` 也接受纯字符串简写。

### 3.2 `host.send_message`

发到任意会话。参数同上（不用 `reply_to`）。需要 `send` 权限。

配置了 `[send] allow_groups` 时，目标群必须在列表里，否则返回 `-32000 permission_denied`。

```json
// 返回
{ "message_id": 10086 }
```

### 3.3 `host.call_onebot`

直通协议端 action。**协议层不枚举接口**，按级别放行。

```json
{
  "action": "set_group_ban",
  "params": { "group_id": 123, "user_id": 456, "duration": 600 }
}

// 返回
{ "data": { } }
```

| 级别 | 例子 | 需要的声明 |
|---|---|---|
| read | `get_group_list` | 无 |
| write | `send_group_msg` | `permissions = ["onebot.write"]` |
| admin | `set_group_ban` | `onebot.admin` |

**发消息类 action 额外受 `send.allow_groups` 白名单约束**——
和 `host.reply` 同一条出口规矩。想不受限地发群消息？用 `host.reply`（走当前会话）。
| dangerous | `get_cookies` | `onebot.dangerous`（默认禁） |

未声明返回 `-32000 permission_denied`。

### 3.4 `host.call_capability`

插件互调。**只接受 `provides` 里声明过的能力名**，不是工具名。
提供者如果声明了 `[[capability]]` 的参数 schema，调用前会校验。

```json
{
  "name": "weather_query",
  "args": { "city": "北京" }
}

// 返回
{ "result": { "ok": true, "data": { ... } } }
```

返回的 `result` 是提供者的原始返回：插件提供者就是它 `plugin.call_tool`
的返回（`{ok, data, error}`），内置 `db.sqlite` 是 `{ok, data}`。
找不到提供者返回 `-32002 unavailable`。

**注意**：能力调用不做调用者权限校验（`uses` 只在加载时检查，见 §7）；
提供者声明了参数 schema 时，未知参数会被拒绝。

### 3.5 `host.check_permission`

问宿主：这个会话能不能用某个插件。AI 调工具前用。

```json
{
  "plugin": "weather",
  "session": { "account": "main", "chat_type": "group", "chat_id": 123456, "user_id": 2849446053, "role": "member" }
}

// 返回
{ "allowed": true }
```

> 宿主在 `CallTool` 里也会自动做同样判定，插件忘了查也绕不过去。

### 3.6 `host.list_access`

拿所有插件的权限配置。

```json
// 返回
{
  "access": {
    "weather": { "allow_users": [], "allow_groups": [], "require_prefix": true, "require_mention": false },
    "echo":    { ... }
  }
}
```

### 3.6b `host.shared_*`

框架托管的跨插件数据。只有声明者能写。

```json
// host.shared_list
{ }  →  { "items": [ { "key": "ai.persona", "owner": "ai-chat", "description": "当前人设", "value": "小鲸鱼" } ] }

// host.shared_get
{ "key": "ai.persona" }  →  { "found": true, "value": "小鲸鱼" }

// host.shared_set（只能写自己声明的 key）
{ "key": "ai.persona", "value": "傲娇" }  →  { "ok": true }
```

### 3.7 `host.list_tools`

拿**这个会话能用**的工具。AI 插件在拼 tools 列表时用，避免把用户无权用的工具暴露给模型。

```json
// 参数（session 传 null 就是全部）
{ "session": { "account": "main", "chat_type": "group", "chat_id": 123456, "user_id": 2849446053 } }

// 返回
{ "tools": [ { "name": "weather_query", "plugin": "weather", "description": "...", "parameters": {} } ] }
```

过滤依据是工具所属插件的权限（同 `host.check_permission`）。

### 3.7b `host.call_tool`

调别的插件声明的 `[[tools]]`（**工具，不是能力**）。带 `session` 时宿主按
工具所属插件的权限过滤；不带 session 且对方插件配了访问限制就直接拒绝。

```json
{
  "name": "weather_query",
  "args": { "city": "北京" },
  "session": { "account": "main", "chat_type": "group", "chat_id": 123456, "user_id": 2849446053, "call_token": "..." }
}

// 返回
{ "result": { "ok": true, "data": { ... } } }
```

8 秒超时；`session.call_token`（`handle_message` 入参里带的）可换到可信会话；
结果超过 32KB 会被截断。

### 3.8 `host.storage_*`

插件私有 KV，存在 `<data_dir>/storage.json`。

```json
// host.storage_get
{ "key": "count:20000" }        →  { "value": 3 }   // 不存在时 value 为 null

// host.storage_set
{ "key": "count:20000", "value": 3 }  →  { }

// host.storage_delete
{ "key": "count:20000" }        →  { }

// host.storage_list
{ "prefix": "count:" }          →  { "keys": ["count:20000"] }
```

### 3.9 `host.log`

notification，无 id，无返回。宿主自动加 `plugin=<id>`。

```json
{ "level": "info", "message": "查询成功", "fields": { "ms": 120 } }
```

---

## 4. 数据结构

### 4.1 Session

| 字段 | 类型 | 说明 |
|---|---|---|
| `account` | string | 账号 id |
| `chat_type` | `group` / `private` | |
| `chat_id` | int64 | 群号或 QQ 号 |
| `user_id` | int64 | 发送者 |
| `message_id` | int64 | 用于 `reply_to` |
| `self_id` | int64 | 机器人自己 |
| `nickname` | string | 群名片优先 |
| `role` | `owner` / `admin` / `member` | 私聊为空 |
| `text` | string | **已去掉命令前缀** |
| `raw_text` | string | 原文 |
| `has_prefix` | bool | |
| `prefix` | string | |
| `mentioned` | bool | 是否 @ 了机器人 |
| `segments` | Segment[] | OneBot 原始消息段 |
| `ts` | int64 | Unix 秒 |
| `reply_id` | int64 | 被引用消息的 ID（消息带 reply 段且段里带 id 时才有，否则省略） |
| `reply` | object | 引用内容 `{user_id, nickname, text, segments}`；宿主用 get_msg 尽力解析，拿不到时**字段整个省略**（JS 里是 undefined，不是 null） |

**引用/回复**：用户在群里回复某条消息再说话时，`reply_id` 通常可解析
（reply 段没带 id 的极少数实现除外），`reply` 是宿主自动调 `get_msg`
换回的被引用内容（AI 插件直接读 `session.reply?.text` 就知道
"他在回复哪句话"）。拿不到时（协议端不支持 get_msg、消息已撤销、
返回内容为空）字段整个省略——判断用 `if (session.reply)` 即可。

### 4.2 Segment

OneBot 11 原生格式，不重新发明：

```json
{ "type": "text", "data": { "text": "你好" } }
{ "type": "at", "data": { "qq": "1102679600" } }
{ "type": "image", "data": { "file": "https://..." } }
{ "type": "reply", "data": { "id": "10086" } }
```

### 4.3 Tool

```json
{
  "name": "weather_query",
  "description": "查询城市天气。用户问天气时调用。",
  "parameters": {
    "type": "object",
    "properties": { "city": { "type": "string" } },
    "required": ["city"]
  }
}
```

### 4.4 SettingSpec

```json
{
  "key": "count",
  "label": "数量",
  "type": "number",
  "default": 5,
  "min": 0,
  "max": 100,
  "options": ["a", "b"]
}
```

`type` ∈ `string | text | textarea | password | number | bool | boolean | select | path`。

`text` / `textarea` 渲染成多行文本框；值仍然是 string。

---

## 5. HTTP 接口

全部需要登录（cookie `qb_session`）。插件设置页也用这些。

### 5.1 页面

| 路径 | 说明 |
|---|---|
| `/login` `POST /login` | 登录 |
| `/logout` | 退出 |
| `/overview` | 概览 |
| `/connect` | 连接设置 |
| `/permissions` | 命令权限 |
| `/plugins` | 插件管理 |
| `/cohost` | cohost 池管理：新建/删除池、手动分配插件；分配后立即按池重载 |
| `/help-settings` | 指令显示 |
| `/tools` | 工具 |
| `/logs` | 日志 |
| `/audit` | 审计 |
| `/chat` | 聊天（人工发消息、看最近消息） |
| `/plugin/<id>/` | 插件详情（设置 + 指令 + 扩展） |
| `/ui/qixin-form.js` | 通用表单组件 |
| `/healthz` | 进程存活检查（不需要登录） |
| `/readyz` | 就绪探针（不需要登录）：账号连接、插件异常计数（不回插件明细）；未就绪返回 503 |

### 5.2 数据接口

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/overview` | 运行时长、账号、插件列表（`plugins` 字段）、功能数 |
| GET | `/api/features` | 功能注册表 |
| GET | `/api/tools` | 工具列表 |
| POST | `/api/tools/call` | 手动调用工具（`name` + `args` JSON） |
| GET | `/api/capabilities` | 能力及其提供者（按优先级） |
| GET | `/api/metrics` | 24h 指标时间序列（5 分钟一窗：消息/回复/插件延迟/内存/队列深度；内存态，重启清零） |
| POST | `/api/capabilities/call` | 直接调能力（验证覆盖关系） |
| GET | `/api/chat/messages?after=N` | 增量拉取最近消息 |
| POST | `/api/chat/send` | 以机器人身份发消息（`chat_type`/`chat_id`/`text`） |
| POST | `/api/chat/inject` | **测试注入**：伪造一条收到的消息走完整分发，插件回复被捕获、不真发到 QQ |
| GET | `/api/logs?after=N` | 增量日志 |
| GET | `/audit` | 审计记录（页面，无独立 JSON 接口） |
| POST | `/api/plugins/upload` | 上传 `.anzu`（multipart） |
| POST | `/api/plugins/{id}/reload` | 重载 |
| POST | `/api/plugins/{id}/enable` | 启用 |
| POST | `/api/plugins/{id}/disable` | 禁用 |
| POST | `/api/plugins/{id}/delete` | 删除（包 + 运行区） |
| GET | `/api/cohost/pools` | 池、成员、PID/RSS、熔断状态 |
| POST | `/api/cohost/pools` | 新建池（`name` / `max_members`，上限默认 12，最大 64） |
| POST | `/api/cohost/pools/update` | 改名/改上限（`id` / `name` / `max_members`；缩容不能低于当前成员数） |
| POST | `/api/cohost/pools/delete` | 删除空池（`id`） |
| POST | `/api/cohost/assign` | 插件移入池（`plugin_id` / `pool_id`，空 = quarantine；立即重载并按池运行） |
| GET | `/api/plugin/{id}/settings` | 返回 `{schema, values}` |
| POST | `/api/plugin/{id}/settings` | 保存并推给插件 |
| POST | `/api/plugin/{id}/settings/restore` | 从轮转备份恢复（`index` 0..3） |
| POST | `/api/connect` | 保存连接配置（重启生效） |
| POST | `/api/password` | 改控制台登录密码（写回 config.toml） |
| POST | `/api/permissions` | 保存插件级权限 |
| POST | `/api/priority` | 保存插件分发优先级（-100~100，大的先处理） |
| POST | `/api/owner` | 保存主人 QQ（框架级最高权限，立即生效） |
| POST | `/api/help` | 保存 /help 显示配置 |

### 5.3c `host.background`：长任务别堵消息

`handle_message` 默认有 30 秒超时。**长任务（爬取、AI 生成、批量操作）应该起后台任务**，
立刻回执，跑完再发结果。

```js
async handle_message({ session }) {
  const text = (session.text ?? '').trim()
  if (text !== 'report') return { handled: false }

  host.background(session, '生成日报', async () => {
    const data = await slowFetch()          // 可能 60 秒
    await host.reply(chatOf(session), data)
  })

  await host.reply(chatOf(session), '正在生成，好了发你')
  return { handled: true }                   // 立刻返回
}
```

**框架做什么**：

| 调用 | 参数 → 返回 | 作用 |
|---|---|---|
| `host.job_begin` | `{id, name, chat?}` → `{ok}` | 登记任务；id 由插件自己生成并保证唯一 |
| `host.job_end` | `{id, error?}` → `{ok}` | 标记完成/失败；未知 id 静默忽略 |

一般不用直接调：SDK 的 `host.background()` 已经把两次调用包好了。

**控制台能看到**：

```
插件        任务        会话            开始       状态
my-plugin   生成日报    group:123456    15:04:05   运行中
```

**限制**：

- **不活过重启**：插件进程死了，后台任务也一起没了
- **要跨重启的任务：把进度/上次时间存 `host.storage`**，启动时续跑
- **插件卸载时**记录被清掉（任务随进程消失）

**这是有意的**：后台任务活在插件进程里，进程没了任务就该没。
真正需要持久化的任务（每天推日报、定时检查订阅）用插件内 `setInterval`/`setTimeout`，并把上次执行时间存 `host.storage`。

### 5.3a `host.ask`：等用户下一条消息

分步交互（绑定账号、设置向导、小游戏）不用自己存状态：

```js
const chat = chatOf(session)   // 已包含 user_id，宿主按"谁回答"投递

const name = await host.ask(chat, '请输入要绑定的名字（30 秒内）：', { timeout_sec: 30 })
if (!name) {
  await host.reply(chat, '超时，已取消')
  return { handled: true }
}
await host.reply(chat, `绑定成功：${name}`)
```

**请求**：

| 参数 | 说明 |
|---|---|
| `chat` | `{chat_type, chat_id, account?}`，发提示语用 |
| `text` | 提示语（空则不发送，只等） |
| `user_id` | 等谁的消息；`chatOf()` 的返回值已带上，只有自己拼 chat 时才需要显式传 |
| `timeout_sec` | 超时秒数，默认 60 |

**返回**：

```json
{"text": "小明", "answered": true}
```

**超时返回 `{"text": "", "answered": false}`**——不报错，插件按"没回答"处理。
（走 SDK 时归一成字符串：Node `host.ask` / Python `host.ask` 超时都返回 `''`；
直连协议才看到这个对象。）

**宿主的处理**：

1. 发提示语
2. 把 `会话+用户` 登记进等待表
3. 该用户的下一条消息**不进插件分发**，直接投给等待者
4. 超时自动清理

**限制**：

- **一次只能答一次**（投递后立刻移除），避免消息错乱
- **多账号安全**：键是 `账号:会话类型:会话ID:用户ID`，
  同一群号在两个账号下是两个独立等待，互不覆盖

**注意**：`host.ask` 会**阻塞插件的 `handle_message`** 直到用户回答或超时。
超时设太长会占住这个插件（其它消息排队），所以别超过 5 分钟。

### 5.3b `session.command`（结构化命令）

插件在 manifest 里声明了 `args` 时，宿主会把解析结果放进 `session.command`：

```json
{
  "command": {
    "id": "weather.current",
    "name": "weather",
    "args": { "city": "上海" },
    "missing": [],
    "raw": ""
  }
}
```

| 字段 | 说明 |
|---|---|
| `id` | manifest 里那条命令的 `id` |
| `name` | 匹配到的命令词（`usage` 的第一个词） |
| `args` | 按 `<占位符>` 顺序解析的位置参数 |
| `missing` | 必填但没给的参数名；空数组表示参数齐了 |
| `raw` | 命令词之后、没被占位符吃掉的剩余文本 |

**没声明 `args` 的插件**：`session.command` 只有 `id`/`name`，`args` 为空，
插件照旧自己解析 `session.text`——**完全向后兼容**。

**匹配规则**：

1. `usage` 的第一个词是命令名（`weather <city>` → `weather`）
2. 和消息的第一个词比，**大小写不敏感**
3. 按 `<占位符>` 顺序从后面取词；`type = "rest"` 吃掉剩余全部

**参数名必须是英文**（TOML 内联表的键不支持中文），中文放 `help`。

### 5.4 测试注入（`/api/chat/inject`）

在控制台里"假装收到一条消息"，走完整分发链路，把插件的回复捕获下来。
**不连真实协议端、不发真实 QQ**。

```bash
POST /api/chat/inject
  text=/echo 你好
  chat_type=group        # group | private，默认 group
  chat_id=0              # 默认 100000
  identity=owner         # owner（主人）| admin（群管理）| member（纯路人）
  user_id=               # 留空时按 identity 取：主人=config 里的 owner
  nickname=测试用户
```

返回：

```json
{
  "ok": true,
  "handled": true,
  "plugin": "echo",
  "plugin_name": "回声",
  "elapsed_ms": 22,
  "text": "echo 你好",
  "has_prefix": true,
  "replies": [
    {"chat_type": "group", "chat_id": 0,
     "segments": [{"type": "text", "data": {"text": "你好"}}]}
  ]
}
```

**返回里带 `trace`（单次消息追踪）**：

```json
"trace": {
  "id": "t1-9a3f",
  "text": "home 列表",
  "total_ms": 21,
  "verdict": "权限（home-assistant）：权限不足",
  "stages": [
    {"name": "收到", "ok": true, "reason": "从控制台注入", "took_ms": 0},
    {"name": "分发", "ok": true, "reason": "开始", "took_ms": 0},
    {"name": "插件", "plugin": "bilibili", "ok": true, "reason": "未认领", "took_ms": 2},
    {"name": "权限", "plugin": "home-assistant", "ok": false, "reason": "权限不足"},
    {"name": "发送", "ok": false, "reason": "没有回复"}
  ]
}
```

**`verdict` 是给人看的一句话结论**，优先报原因（权限/插件），不是结果（没有回复）。

**三层保护，确保不会误发**：

1. `Queue.Mute()` 全局静音——即使捕获失效，所有 `Send` 直接丢弃
2. `Queue.BeginCapture()` 捕获——把想发的消息收下来给前端
3. `account = "__inject_test__"` 占位符——就算前两层都漏了，也找不到真实连接

**捕获期间不记聊天记录**（`IsCapturing()` 时跳过 `chatLog.Add`），
所以测试消息不会污染 `/chat` 的历史。

**注意**：注入会真的走一遍插件调用——如果插件有副作用（写文件、调外部 API），
**副作用是真实的**。只有"发消息"这一步被拦住了。

### 5.3 示例：从插件页面调接口

```js
// 读设置
const { schema, values } = await (await fetch('/api/plugin/weather/settings')).json()

// 存设置
await fetch('/api/plugin/weather/settings', {
  method: 'POST',
  body: new URLSearchParams({ unit: 'fahrenheit' }),
})

// 调工具
const r = await fetch('/api/tools/call', {
  method: 'POST',
  body: new URLSearchParams({ name: 'weather_query', args: '{"city":"北京"}' }),
})
```

---

## 6. 错误码

| code | 名称 | 含义 |
|---|---|---|
| -32700 | parse_error | 帧不是合法 JSON |
| -32600 | invalid_request | 缺 method/id |
| -32601 | method_not_found | 未知方法 |
| -32602 | invalid_params | 参数校验失败 |
| -32603 | internal_error | 插件内部异常 |
| -32000 | permission_denied | 权限不足 |
| -32001 | timeout | 超时 |
| -32002 | unavailable | 依赖缺失 |
| -32003 | conflict | 名称冲突 |
| -32004 | version_mismatch | apiVersion 不兼容 |
| -32005 | rate_limited | 触发限流 |

**不要用 error 表达业务失败**。工具查不到天气是 `{ok:false}`，不是 error。

---

## 7. 权限检查点

宿主只在四个地方判定：

| 检查点 | 内容 |
|---|---|
| 消息分发 | 用户/群能否触发这个插件（含前缀、@ 门禁） |
| `host.send_message` / `host.reply` | 插件能否发到这个群 |
| `host.call_tool` / `host.list_tools` | 带 session 时按工具所属插件的权限过滤 |
| `host.call_onebot` | action 是否在该插件允许级别内 |

**`host.call_capability` 不做调用者权限校验**——能力是插件互信设计，
`uses` 声明只在加载时用来标 degraded，调用时不强制。需要权限边界的
场景（AI 可调、跨插件）用 `[[tools]]` + `host.call_tool`。

所有拒绝都会写日志，带插件名和原因。

## 附：`[cooldown]` 配置

```toml
[cooldown]
per_user   = "3s"
per_group  = "1s"
per_plugin = "0s"
```

| 字段 | 作用 |
|---|---|
| `per_user` | 同一用户对同一插件的最小间隔 |
| `per_group` | 同一群对同一插件的最小间隔 |
| `per_plugin` | 整个插件的全局最小间隔 |

**冷却期静默跳过**（不回复"慢点"），追踪里显示 `✗ 冷却 — 还需 N 秒`。

**不写 `[cooldown]` 就是完全不限制**——框架没有内置默认值，上面的数值只是示例，防刷屏要自己声明。

## 附：`[timeouts]` 配置

```toml
[timeouts]
init = "10s"          # 启动握手。注意：目前是占位，握手固定 10 秒，此值尚未生效
capability = "15s"    # 能力调用（默认 15 秒）
message = "30s"       # 单条消息（默认 30 秒）
call = "30m"          # 单次宿主调用的兜底（默认 30 分钟）
```

**`init` 只管启动握手**——不会截断 `host.ask` 的等待；`call` 是所有
插件 → 宿主调用的兜底（`host.ask` 长等待靠它兜住）。

重依赖的服务插件可以放宽；轻插件保持默认。

## 附：`[[capability]]` 的 `max_concurrent`

```toml
[[capability]]
name = "my.thing"
max_concurrent = 2
```

限制同时有几个调用打向这个**能力**（按 `name` 区分，同一服务的不同能力各自计数）。
满了排队，排队超时返回 `CodeRateLimited`。

**`retry`**（默认安全）：

```toml
[[capability]]
name = "my.thing"
retry = "always"      # 失败后切换下一个提供者（要求幂等）
```

默认**不重试**——能力可能有副作用，超时后原提供者未必停，换了可能执行两次。

**Python 服务建议设为 1**（同步逐条处理）。

## 附：`load_order` 加载顺序

```toml
id = "canvas"
kind = "service"
load_order = 10      # 小的先加载
```

只在同一批（service 那批 / 普通那批）内生效。同值按 id 字母序。

**用途**：服务 B 的 `initialize` 要调服务 A 时，把 A 设小。

**和 `[[commands]].priority` 无关**：那个管消息分发顺序。
