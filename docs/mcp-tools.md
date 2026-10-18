# MCP 工具参考

Termcp 通过 **SSE**、**Streamable HTTP** 与 **stdio 桥**三种方式暴露 MCP 工具（SSE 与 Streamable HTTP 共用同一监听端口，stdio 桥是本地子进程、转发到其中之一）。

## 工具面：一个入口 `term`

`tools/list` 默认只返回**一个**工具：

```jsonc
{
  "name": "term",
  "inputSchema": {
    "type": "object",
    "properties": {
      "cmd":        { "type": "string",  "description": "一条终端指令…留空 = 引导" },
      "timeout":    { "type": "number",  "default": 2 },
      "bytes_limit":{ "type": "number",  "default": 8192 },
      "lines_limit":{ "type": "number",  "default": 200 },
      "total_bytes_limit": { "type": "number", "default": 32768 },
      "on_time_overflow":  { "type": "string", "default": "truncate" },
      "on_bytes_overflow": { "type": "string", "default": "truncate" },
      "on_lines_overflow": { "type": "string", "default": "truncate" },
      "pipe_to":    { "type": "string"  },
      "learn_mode": { "type": "boolean", "default": false }
    }
  }
}
```

序列化体积约 **2.0 KB**（历史工具面是 32 个工具、约 22–25 KB，每轮都进模型上下文）。**能力增长发生在命令空间，不发生在 `tools/list`**：装一个 MCP server、一条技能扩展、十条别名之后，工具面不变。

`cmd` **非必填**。空调用合法且便宜：返回用法摘要 + 当前有什么可操作（谁在跑、装了什么）。这是模型"第一次连上"和"迷路了"时的第一个动作。

**没有第二套工具面。** 32 个特化工具连同它们的 schema、描述、参数压缩表和 `--mcp-defer-tools*` / `--mcp-specialised-tools` 开关一并删除。能力全在指令里，handler 仍在（`term` 就是渲染它们的），所以能力一个没少。

### 三个窗口轴

| 参数 | 轴 | 语义 |
|------|----|------|
| `timeout` | 时间 | 最多等多少秒。命令提前结束就提前返回；`0` = 不等，只取当前已有 |
| `bytes_limit` / `lines_limit` | 空间 | 一次返回的原始字节 / 完整行上限。`0` = 不发字节，只给 handle 与 range，**不消费流** |
| `total_bytes_limit` | 整个回复 | 所有 slot 合计的绝对上限，超出部分截断并标 `206` |

窗口**只约束返回多少，不改变命令做什么**。溢出策略 `on_*_overflow`：`truncate`（默认，返回头部）或 `fold`（返回零字节）。截断不是失败：`206` 仍是成功。

### 信封

每次调用返回一段 HTTP 风味文本，一个 slot = 一条响应：

```
TERM/1 200 OK
Path: run a1f3:1 ls -la
Content-Range: bytes 0-1023/2048

total 8
drwxr-xr-x ...
```

- 状态行：`TERM/1 <code> <reason>`。`2xx` 成功、`4xx` 参数/状态错误、`5xx` 命令本身失败。`isError` 恰为 `code >= 400`。
- 头部按固定顺序出现，**只出现有意义的**：`Path`（总是，显示实际执行的指令，别名已展开）、`Status`、`Exit-Code`、`Handle`、`Content-Range`、`Error-Code`、`Idempotent`、`Usage`。
- body 是原始字节。**同一个值**的 JSON 投影在 `structuredContent`：`protocol` / `code` / `reason` 加每个头部同名键，再加 `body`（原样，不省略）——只渲染结构化投影的客户端也不会丢字节。
- 批量目标（`run a1f3:1,b7c2:1 …`）返回一个 slot per 目标，按顺序拼接。`isError` 是任一 slot 失败。

`Content-Range` 用 RFC 9110 形式，`end` 包含在内，所以 `end+1` 就是下一次的 `--cursor`。被 `total_bytes_limit` 截断的 slot 会同时改写这个头——它必须描述 body **实际**携带的字节，否则下一次读会跳过一段。

### 学习与发现

- `help` 列出全部指令；`help run` 给一条指令的一行用法，再加一段该指令的细节（含默认值，例如 `read`、`close`、`watch`、`notify`）。
- 任何 `400` 都带 `Usage`（未知指令带整张表），因为出错正是调用方不知道形状的时刻。
- `learn_mode=true` 把命中的指令用法附在 slot 上，**每条指令一次**（批量里不重复）。成功回复默认**不带** `Usage`：已经会了就不该付费。
- `Usage` 从不说明状态，只说明语法：它回答"怎么用"，不回答"发生了什么"。

## 指令语法

一条 `cmd` 是一行 shell 风味文本：`<directive> <target> <flags/payload>`。`;` 或换行分隔多条指令，**每个 statement 一个 slot**；`&&` / `|` 留在 payload 里（那是一条命令，调用方要的是它的结果）。

解析用 `mvdan.cc/sh/v3/syntax`（只解析、不求值）：`$HOME` / `$(cmd)` / 反引号原样进入 shell，由 shell 求值。**payload 是按源码切片取的**，所以 `'a'"b"'c'` 逐字节原样，不会被归一化、重新加引号或丢空格。

- 目标（`<handle>`）是第二个词。flag 必须写在目标**之后**：`key --repeat 3 a1f3:1 up` 会被拒绝并给 `Usage`，否则 `--repeat` 会静默变成目标。
- 定位符（`termcp://#ctf-1:2`）与裸 id 都可作目标。
- 逗号不触发分词：`a1f3:1,b7c2:1` 是**一个**词，即一次批量。

### 会话与通道

| 指令 | 说明 |
|------|------|
| `pipe <entry\|session> [--name …] [--on-exit keep\|close] --argv <cmd> [args…]` | 开一个 pipe 通道（无 TTY、无回显、跑完即退）。`--argv` 是 **argv**（词表）不是命令行：按 shell 引用规则分词、不做展开，`;`/`\|`/`>` 不是运算符；整段再包一层引号就是一个程序名（和终端一致）。**`--argv` 必须放最后**，`--name`/`--on-exit` 写在它前面；要 shell 语法就 `--argv sh -c '…'` 或写脚本文件 |
| `pty <entry\|session> [--name …] [--size 24x80] --argv <cmd> [args…]` | 开一个 pty 通道（交互式；不传 `--argv` 就是登录 shell）。`--argv` 规则同 `pipe` |
| `ps [--shells] [--all]` | 当前有什么在跑。终端列格式，每行第一个字段就是可直接复制的 handle |
| `info <session\|handle>` | `key: value` 一行一个事实 |
| `resize <handle> <rows> <cols>` | 改 pty 尺寸 |
| `close [--force] [--grace N] <handle\|session>` | 关通道或会话。`--grace` 默认 5 秒（进程退出的等待时间）；`--force` 只对会话生效 |
| `rm <session>` | 彻底删除会话 |
| `detect` | 探测本机的交互式 shell |

### 输入与输出

| 指令 | 说明 |
|------|------|
| `run <handle> <payload>` | 把 payload 送进 shell 并按回车，然后读窗口。**从提交点开始读**，不会把上一条命令未读的尾巴当成本次结果 |
| `type <handle> <payload>` | 只打字，不回车 |
| `key <handle> <key> [--repeat N]` | 按键（`enter` / `up` / `ctrl+c` …） |
| `read <handle> [--cursor N\|--from-line N\|--tail N] [--stream o\|e] [--raw]` | 读一个窗口，不发送任何东西。默认：活通道读新增字节，已关闭通道读尾部；`--stream` 默认 stdout 与 stderr 都保留 |

`run` 在 pty 上等满时间窗 `timeout`（默认 2s）后返回窗口——shell 不会汇报"命令结束"，所以窗口就是采样时长；在 pipe 上等进程退出（`timeout` 是上限）。通道已关闭时，`Status` / `Exit-Code` 出现在**每一次**关于它的回复里（状态是状态，不是事件），非零退出让该回复成为 `500`，但**字节和 range 照旧返回**。

### 观察与等待

| 指令 | 说明 |
|------|------|
| `span <handle>` | 会话输出区段索引（时间、字节偏移、行号） |
| `reader <handle>` / `reader <handle> --drop <id>` | 注册/释放输出游标 |
| `watch <handle> [--event output\|exit\|silence] [--for N] [--channel resource\|sampling]` / `watch --off <rule>` / `watch --list` | 等一件事发生：输出、退出、或静默 N 秒。默认 `--event output`、`--for 3`；`--channel` 省略时客户端声明了 sampling 就用 sampling，否则 resource |
| `notify [--level info\|success\|warn\|error] [--sticky] [--on <session>] <body>` | 通过 Web UI 通知人类（toast + 系统通知）。**问人类任何东西之前先调用它**，然后停下等，不要轮询。默认 `--level info`，toast 显示 10 秒（`--sticky` 常驻） |

### 远端面

| 指令 | 说明 |
|------|------|
| `fwd --action local\|remote\|dynamic\|list\|close … <session>` | SSH 端口转发 |
| `file <session> --action read\|write\|stat\|delete\|rename\|mkdir\|urls\|perm\|link\|fs\|getwd …` | SFTP 操作 |
| `profile list\|create\|edit\|copy\|delete …` | SSH 连接配置 |
| `ext list\|get\|create\|update\|delete\|retry …` | 扩展层（MCP server / 技能窗口 / prompt 终端） |
| `mcp open <url> [--sse]` / `mcp tools\|send\|recv\|call\|close <handle> …` | 把任意第三方 MCP server 当成一条通道驱动。HTTP 端点不消耗通道 |
| `alias <name> <directive>` / `alias` / `alias <name> --off` | 给指令起名字。展开一次，`Path` 显示展开后的模样 |
| `help [directive]` | 指令表 / 单条用法 |

---

## 对接方式（传输）

| 传输 | 端点 | 配套 | 说明 |
|------|------|------|------|
| SSE | `GET /sse` | `POST /message` | 客户端只配置 `/sse`；SDK 自动用 `/message` 发 JSON-RPC |
| Streamable HTTP | `/stream` | — | 单路径；不要拼 `/sse` 或 `/message` |
| stdio（桥） | 本地子进程 | `termcp stdio` | 只认 stdio 的客户端。桥把 stdin/stdout 上的 MCP 消息转发到上表任一 HTTP 端点，`termcp daemon stdio` 则先拉起实例再进桥；详见 README 的方式 C |

- SSE：`http://<host>:18765/sse`（Claude：`--transport sse` / `type: "sse"`）
- Streamable HTTP：`http://<host>:18765/stream`（Claude：`--transport http` / `type: "http"`）

可复制配置与 HTTP API 速查：Web UI **`/api.html`**。完整客户端样例见 `README.md` / `README.zh.md`。同一批会话也可用实例自带的 Agent Skill 驱动（`/skills.md` 安装一次，配 `GET /api/resolve` 解析 `termcp://` 定位符，纯 curl）。

## Resources & Prompts

Besides tools, the server publishes its own HTTP API reference and curl skill as
MCP resources, and a `learn-api` prompt:

- Resources: `<origin>/api.md` (REST reference) and `<origin>/skills.md` (curl skill).
  URIs are the instance's real HTTP addresses, so the same string works for `curl`.
- Prompt: `learn-api` (optional argument `task`) — primes an agent with `/api.md`
  and `/skills.md` before it scripts against Termcp over REST.

What the `term` description advertises — which address, from what, and why not on the
instructions or a resource — is documented once, in `docs/api.md` §2
*Agent-facing documents (HTTP + MCP resources)*; this file does not repeat it. The
resource URIs and the `learn-api` prompt follow the same per-request origin.

两个文档端点在开启鉴权后仍可**无凭据**获取（仅 GET/HEAD，纯静态、无数据）；其余所有面（REST/MCP/WS/Web UI）依旧要求 token。

## 调用方命名空间（namespace）

多个 agent 共用一个实例时，默认它们看到的是同一批会话（全局单例），于是两个 agent 很容易互相碰到对方的终端。给 MCP 请求加一个请求头即可把调用方圈进自己的命名空间：

```
X-Termcp-Namespace: alice
```

| 行为 | 未声明（默认） | 声明了 `alice` |
|------|----------------|----------------|
| `ps` | 所有非基础设施会话 | 只有 `alice` 自己开的会话 |
| `pipe` / `pty` 起新会话 | 普通会话 | 会话打上 `ns:alice` 标记（随 manifest 持久化） |
| 按 handle 操作 | 任何会话（含扩展会话，只要手里有 handle） | **只有 `ns:alice`**，否则 `forbidden` |
| 扩展/基础设施会话 | 可（持有 handle 时） | 拒绝 |

未声明命名空间的调用方是**实例所有者**（浏览器、stdio 桥、没设头的 agent），不是租户：它保持今天的行为（基础设施会话不进 `ps`，但已有 handle 仍可用，这正是扩展文档承诺的）。这个特性解决的是"两个 agent 互相冲突"，而不是"向所有者隐藏 agent"。

名字限字母/数字/`-_.`，最长 64；非法值不会静默降级成"无命名空间"（那会反而拿到共享视图），而是拒绝一切会话。`ns:` 前缀是内部的，客户端只传裸名。stdio 桥默认不带头，要隔离就自己包一层 HTTP 客户端或在桥上加头（`Config.Headers` 已支持）。

## 工具懒加载：已删除

`defer_loading` 存在的意义是「工具太多，先别给 schema」。现在只有一个工具，这个问题不存在了，所以 `--mcp-defer-tools` / `--mcp-defer-tools-level` 与其分类表（`deferredTools` / `allLevelCore`）一并删除——保留一个永远只有一个成员的分类体系是纯负债。

顺带说明它为什么本来就默认关闭：不认识 `defer_loading` 的客户端、或经网关转发而被丢弃标记的链路（实测 **Codex 0.155.1 经 AxonHub** 会把带标记的工具整个吞掉，模型侧看到「零工具」），会让这些工具**从模型视野里直接消失**。单工具面下这个问题彻底消失。

## 资源 URL 寻址（termcp://）

这些 URL 是**复制给 AI 用的定位符**：Web UI 各处的复制按钮（entry 卡片、session 卡片、终端标题、每个 shell 频道标签）一键复制后，直接粘进与 AI 的对话或任务描述中，AI 就能精确定位你说的是**哪个连接 / 哪个会话 / 会话里的第几个频道**，不用再费口舌描述。点击复制按钮不会触发连接、切换频道或关闭窗口。

| 层级 | URL 形式 | 例子 |
|------|----------|------|
| entry（连接配置） | `termcp://[entry名]` | `termcp://internal` |
| session（会话） | `termcp://#[会话id]`（**短形式，复制按钮统一输出此形式**） | `termcp://#ctf-1` |
| shell（频道） | `termcp://#[会话id]:[序号]` | `termcp://#ctf-1:2` |

- `[会话id]` 就是会话卡片上的等宽小字（不带 `session-` 前缀）；`[序号]` 是**频道在会话内的编号**：从 1 起、在频道创建时由服务端分配、**终生不变且不复用**——关掉靠前的频道不会让后面的编号前移，因此已复制的定位符始终指向同一个频道，而编号已被关闭的频道会解析失败（不会滑到邻居身上）。它与频道标签 `shell-1`/`shell-2` 一致；无序号 = 首个 shell。
- **`term` 的 handle 就是短形式**：`ps` 输出的第一列（`a1f3:1`）可以直接作为下一个指令的目标，也可以直接粘进 `termcp://#` 后面换取完整定位符。
- **REST 侧也能解析**：`GET /api/resolve?url=termcp://...` 返回 `kind=entry|session|shell` 与对应 `ssh_config` / `session_id` / `shell_id`（纯 curl 的 agent 用；语法解析器与 MCP 共用 `internal/locator`）。
- **MCP 侧在所有 id / profile 参数位置都接受定位符**（`session_id` / `shell_id` / `ssh_config` 三者一致）：`pipe termcp://mac`、`close termcp://#ctf-1`、`type termcp://#ctf-1:2 …` 等都无需先解析成裸 id，一次调用直达。另外 `session_id` 也接受**裸 shell id**（shell 属于哪个会话是确定的），`shell_id` 也接受**裸 session id**（= 首个频道）。
- **返回的 handle 总是解析后的真实 id**，不会把传入的定位符原样回显。这很重要：forward 的 `session_id`、通知规则的 `shell_id` 会被服务端作为内部索引使用（会话 DEAD 时级联关闭/清理都按真实 id 匹配），回显定位符会让这些资源无法被级联回收、通知 URI 也会变成一个不存在的名字。
- **裸名字不是定位符**：没有 scheme、也没有 `#` 的 `session-<id>` 保持作为 profile 名/裸 id 理解——profile 名与会话 id 共用同一命名空间，`session-foo` 这样的 profile 必须继续可用。只有显式的会话写法（`#<id>`、`termcp://#<id>`、`termcp://<entry>#<id>`）才按会话解析，并在此剥掉 `session-` 前缀（`#session-foo` = 会话 `foo`）。
- **HTTP 侧**：路径/查询参数只接受裸 id，定位符先用 `GET /api/resolve` 换成 id（定位符里的 `#`/`:` 本来也无法安全地放进 URL 路径）；写操作的 JSON body 里的 `ssh_config` 字段接受 entry 定位符。
- 兼容旧形式 `termcp://[entry名]#[会话id]`：entry 前缀被忽略（会话名与 entry 名无关），以会话 id 为准。
- **已关闭（DEAD）会话**：写操作不接受定位到已关闭会话，会返回带提示的错误——已关闭会话是只读的，用 `read`（`--tail N` / `--cursor N` 翻页）读取。定位符解析失败（如 `termcp://#sid:0`）返回 `400` + `invalid_argument` 并附具体原因。
- 通知通道专用格式：`watch --event output` 的资源 uri 固定为 `termcp://shells/<shell_id>`（仅作事件载体，不是可用定位符）。

---

## 错误码

失败的 slot 是 `4xx` / `5xx`，`Error-Code` 头给出稳定的 snake_case 错误码，body 是人类可读说明：

```
TERM/1 400 Bad Request
Path: read a1f3:9
Error-Code: shell_not_found
Usage: read <handle> [--cursor N|--from-line N|--tail N] [--stream o|e] [--raw]

shell "a1f3:9" not found
```

| 错误码 | 含义 | 典型处理 |
|--------|------|----------|
| `invalid_argument` | 参数/语法非法 | 看 `Usage` 修正后重试 |
| `unknown_directive` | 没有这条指令 | `help` 取全表 |
| `session_not_found` | 无此会话，或已关闭 | `ps` 复核 |
| `shell_not_found` | 无此 handle（可能已 `close`） | `ps --shells` 复核 |
| `session_not_running` | 会话已 DEAD，只读 | 重新 `pipe` / `pty` |
| `reader_not_registered` | `reader_id` 未在该通道注册 | 先 `reader <handle>` |
| `forward_not_found` | 无此 forward | `fwd --action list` |
| `ssh_config_not_found` | 无此 profile | `profile --action list` |
| `rule_not_found` | 无此通知规则 | `watch --list` |
| `conflict` | 资源已存在 | 改用 edit / 其它名称 |
| `not_configured` | 功能未启用/未初始化 | 检查服务启动参数 |
| `forbidden` | 该能力不在会话模板的 `allow`/`paths` 白名单内（扩展会话） | **不要重试**：`ext get` 看模板允许什么 |
| `connection_failed` | 拨号/连接失败 | 检查网络与目标地址 |
| `command_failed` | 命令以非零退出 | 看 `Exit-Code` 与 body |
| `operation_failed` | 其它操作失败 | 查看 body 详情 |
| `internal_error` | 服务端内部错误 | 查看服务端日志 |

---

## 会话生命周期

```
ps                                  # 有什么可操作
  → pipe internal --argv sh          → Handle: a1f3:1
      → run a1f3:1 ls -la
      → type a1f3:1 echo hi
      → key a1f3:1 enter
      → read a1f3:1 --tail 40
  → pty internal                     → Handle: b7c2:1   (登录 shell)
  → close a1f3:1                     # 关通道不关会话：fwd / file 照常
      → fwd --action local --session … / file … --action read …
  → close --force <session>          # 关闭会话（转入 DEAD）
```

`pipe` / `pty` 的 `on-exit` 与历史上 `on_exit` 语义一致：`keep`（默认）保留会话供复用，`close` 让一次性命令跑完后自动归档。

---

## 能力层（实现说明）

准入判断只有一份：`internal/capability` 的一张表，每行一个能力（`id`、`approval.Kind`、`paths`、`gate`、`idempotent`）。这三件事——白名单、命名空间、审阅门控——曾经在 MCP、Web UI 和扩展模板里各推导一遍，现在都问这张表：

- `internal/session` 的 `Restriction.allow` 存**能力 id**（旧 manifest 里的工具名仍然可用，经 `capability.IDForTool` 转换）。
- `internal/mcp/admission.go` 的 `checkAdmission` 是 MCP 唯一的准入点，在 handler 之前跑；`internal/webui` 的拒绝渲染也按能力 id。
- 扩展模板的 `allow` / `paths` 写能力 id（`extensions/preset.go`）。

因此"同一操作经 `term`、经特化工具、经 REST、经 WebSocket 产生相同状态"不是靠自律，而是因为**判定只有一处**。
