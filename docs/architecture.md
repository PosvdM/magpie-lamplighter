# 架构

Lamplighter 是一个 Go 程序。Magpie 的插件 `magpie-lamplighter`（npm 包）在 Magpie 的插件宿主（Bun）中启动它；它通过 Magpie 网关（环境变量 `MAGPIE_ADDR`）和 Magpie 配置目录中的文件读取账号、额度和凭证，自己向厂商查询额度，经网关点火，并提供页面、HTTP 接口和 MCP。

## 模块

| 路径 | 职责 |
| --- | --- |
| `main.go` | 程序入口：打开数据目录和配置，启动引擎和内部监听，收到 SIGINT 或 SIGTERM 时退出 |
| `internal/server` | HTTP 路由：页面、状态和操作接口、配置读写、插件宿主的 tool 调用；把配置交给引擎和对外监听 |
| `internal/host` | Lamplighter 与 Magpie 之间的接口 `host.Host` 及其数据类型，见“与 Magpie 的接口” |
| `internal/magpie` | `host.Host` 的实现：网关请求、Magpie 文件的读取 |
| `internal/engine` | 后台循环：主动查询、被动数据、点火、通知、历史和状态页数据；`mcp.go` 提供模型可读 DTO 和额度预测 |
| `internal/mcp` | 对外监听：页面和接口的 `admin_key` 鉴权，`/mcp` 的 MCP Streamable HTTP 协议、`mcp_key` 鉴权和 Origin 校验 |
| `internal/config` | 配置解析、默认值和取值范围；配置文件的读写（`Store`）和数据目录 |
| `internal/providers` | 每个服务一个文件：额度请求和解析、响应头解析、点火请求、候选模型、默认设置；注册表，见下文“服务” |
| `internal/quota` | 额度窗口和上游请求的类型，窗口种类判断，时间解析 |
| `internal/egress` | 按账号的代理发送额度请求 |
| `internal/models` | 缓存网关的模型列表，提供按新旧排列候选模型的工具 |
| `internal/ignite` | 点火时间计算、滑动重置判断、失败分类和失败保护 |
| `internal/failure` | 把点火和额度查询的上游错误分成凭证失效、账号停用、限流，并提炼一句话摘要 |
| `internal/notify` | Bark 和 webhook 发送、额度提醒、Did Codex Reset 转发 |
| `internal/release` | 读取 GitHub 上的最新 Release，比较版本号 |
| `internal/store` | `state.json`、额度历史和实例锁 |
| `internal/web` | 管理页面，HTML、CSS、JS 分文件编辑，运行时合成一个文档 |
| `npm/` | Magpie 插件外壳（npm 包 `magpie-lamplighter`）：Bun 入口启动程序并提供 tools，页面启动文件，见“插件外壳” |

## 进程和数据目录

`lamplighter -listen <地址>` 在 `-listen` 上开内部监听（默认 `127.0.0.1:8317`），只应绑定回环地址：内部监听不要求密钥，地址不是回环地址时 `main.go` 记录一条警告；请求的 Host 不是回环地址时返回 403，见“HTTP 接口”的“内部监听”。需要从外部访问时使用对外监听。数据目录依次取 `-data-dir`、`LAMPLIGHTER_DATA_DIR`、`$XDG_STATE_HOME/lamplighter`（Magpie 的 Docker 镜像中为 `/config/state/lamplighter`，升级后保留）、Windows 的 `%LOCALAPPDATA%\lamplighter`、`~/.local/state/lamplighter`（`config.DataDir`）。目录中有：

- `config.json`：配置，权限 600。文件不存在时使用默认值；存在但无法解析时程序退出并报告文件路径，不覆盖它。
- `state.json`、`history/`、`instance.lock`：见下文“数据”。

## 插件外壳

npm 包 `magpie-lamplighter` 是 Go 程序在 Magpie 中的外壳，源码在 `npm/`，`package.json` 的 `magpie` 字段声明页面和端口选项。包中带有 linux、darwin、windows 各 amd64 和 arm64 的程序（`dist/bin/<GOOS>-<GOARCH>/lamplighter`，构建方法见[开发](./development.md#构建)）。Magpie 用 `bun add --ignore-scripts` 安装插件，不运行安装脚本，所以程序必须随包发布，不能在安装时下载。

**启动**：插件宿主（Bun）加载 `npm/index.js` 的默认导出 `{id, server}`，并以 `plugins.json` 中插件的 `options` 调用 `server`。宿主把模块导出的每个函数都当作插件函数，只有采用默认导出的写法，`lamplighter.js` 中的辅助函数才不会被当作插件函数加载。`server` 按 `process.platform` 和 `process.arch` 找到程序（缺少执行权限时补上），以 `-listen 127.0.0.1:<端口>` 启动，然后返回 tools；同一个宿主中只启动一次。端口取 `options.port`（1～65535 的整数，或这样的数字字符串），没有时为 8317，与 `main.go` 中 `-listen` 的默认值相同（`npm/test/package.test.mjs` 检查两处一致）。Magpie 的 CLI 进程（如 `magpie plugin update`）会另起一个宿主，并把 `options.port` 换成一个空闲端口，所以端口必须从选项读取。

**环境变量**：程序继承宿主的环境（`childEnv`），另外：

- 宿主的工作目录就是 Magpie 的配置目录（便携模式也是），环境中没有 `MAGPIE_CONFIG_DIR` 时把它设为宿主的工作目录。
- `MAGPIE_ADDR` 原样传递。网关不要求回环地址上的请求带密钥，因此不传密钥。
- Magpie 把宿主环境中的 `HTTPS_PROXY`、`HTTP_PROXY`、`ALL_PROXY`、`NO_PROXY` 改名为 `MAGPIE_` 开头的同名变量（Magpie 源码 `internal/plugin/host.go` 的 `hostEnv`）。这些变量的值是 Magpie 自己的请求所用的代理（`netproxy.Env`：Magpie 中设置的代理，否则是系统代理；设为直连时没有；SOCKS5 代理已桥接为 HTTP 代理）。改名是因为 Bun 只在启动时读一次 `*_PROXY`，之后所有 fetch 都经过它，按服务设置的直连就无法生效；改名后由 `host.js` 为每个 fetch 单独选择代理，并没有禁止插件使用代理的意图。`host.js` 也按同样的方式把代理传给插件启动的程序（`proxied`），所以 `childEnv` 把 `MAGPIE_HTTPS_PROXY`、`MAGPIE_HTTP_PROXY`（缺少时用 `MAGPIE_ALL_PROXY` 代替）和 `MAGPIE_NO_PROXY` 映射回 `HTTPS_PROXY`、`HTTP_PROXY`、`NO_PROXY`；环境中已有同名变量（不区分大小写）时保留原值。Go 的代理设置不作用于回环地址，所以访问网关时不经过代理；账号自己的代理见“网络出口”。

**生命周期**：`Supervisor` 让程序保持运行。程序自行退出时，先等 1 秒再重新启动；每次快速退出后等待时间加倍，最长 30 秒；程序连续运行 60 秒以上再退出时，等待时间回到 1 秒。快速退出通常是因为端口被占用：Magpie 换下内存占用过高的宿主时，新宿主开始接收调用，旧宿主及其程序要等调用结束才退出。宿主退出时（`process` 的 `exit` 事件），外壳向程序发送 SIGTERM（Windows 上直接结束进程），程序收到后正常关闭。此外，Magpie 停止宿主时会结束整个进程树（Unix 上是宿主所在的会话，Windows 上是作业对象），程序不会残留。程序的标准输入为空，标准输出和标准错误输出交给宿主，Magpie 把宿主的标准错误输出写入日志。

**tools**：`npm/tools.js` 定义的工具与 `internal/mcp` 的 `toolDefinitions` 一一对应，名称、参数和说明相同（`npm/test/tools.test.mjs` 对照 `internal/mcp/server.go` 检查名称和参数）。参数以 JSON Schema 声明，Magpie 照原样列出、不做校验，所以 `toolArguments` 按 MCP 服务的规则检查：拒绝工具不接受的参数（`Unknown argument: …`），`limit` 必须是整数，其余参数必须是字符串。`execute` 把参数转成字符串对象（`limit` 写成十进制数字），发到内部监听的 `POST /api/tools/{name}`。返回 `{"result": …}` 时把结果写成 JSON 文本作为工具的回答，返回 `{"error": "…"}` 时抛出该错误，Magpie 将其作为调用失败返回。内部监听拒绝连接时（程序正在启动或重新启动），每 250 毫秒重试一次，最多等 10 秒。Magpie 的 tool 超时为 55 秒，外壳在 54 秒时放弃请求，内部监听给工具 50 秒。Magpie 以插件的短名称加工具名命名这些工具（如 `magpie-lamplighter_list_quotas`），单独提供这些工具的 MCP 地址是 `/mcp/magpie/tools/magpie-lamplighter`。

**页面**：`magpie.page.file` 指向 `npm/page/index.html`，它先加载 Magpie 的 `_magpie/bridge.js`，再由 `open.js` 跳转到 Lamplighter 自己的页面：经 `magpie web` 打开时，桥接脚本提供经 Magpie 反向代理的内部监听地址 `magpie.service`（来自 `magpie.page.service` 的 `{port: 8317, option: "port"}`），页面跳转到这个地址；在 Magpie 桌面窗口中没有这个地址，页面读取插件的 `port` 选项，跳转到 `http://127.0.0.1:<端口>/`。桥接脚本 3 秒内没有响应时使用默认端口。页面的沙箱策略不允许内联脚本，所以脚本放在单独的文件中。`magpie.options` 中的 `{"port": 8317}` 是 Magpie 插件页面“Options”编辑框中显示的示例。

## 与 Magpie 的接口

引擎只通过 `host.Host` 访问 Magpie，测试使用假的实现。`internal/magpie` 按下表实现它：

| 方法 | 来源 |
| --- | --- |
| `Accounts` | 网关的 `GET /v1/magpie/accounts`，只取 `kind` 为 `account` 的条目（API key 没有额度窗口）。每个账号的 `pin`（`<服务>/<id>`）是 Lamplighter 中账号的唯一键：`state.json`、`account_weights`、`account_order`、`account_names` 都以它为键，点火时作为 `X-Magpie-Account` 发送。`on` 为假的账号不监控。`User` 原样取 `user`（Magpie 的账号名，Magpie 按它的小写保存每个账号的设置）；`Email` 取 `user` 中的邮箱：Codex 的 Team、Business 等工作区席位和 Claude 的 Team、Enterprise 席位的 `user` 带上工作区（如 `a@b.com · Team`），`Email` 取 `·` 之前的部分，用于显示和打码 |
| `Credential` | Magpie 配置目录（`MAGPIE_CONFIG_DIR`，否则 `$XDG_CONFIG_HOME/magpie`，否则 `~/.config/magpie`）中的文件，见下文“凭证” |
| `KeptQuotas` | 网关的 `GET /v1/magpie/quotas?kept=1`：Magpie 记下的最近读数，不向厂商发请求。见下文“额度记录” |
| `RefreshQuota` | 网关的 `POST /v1/magpie/quotas/refresh?provider=<服务>&user=<账号名>`。Claude 的刷新会运行 Claude Code 的 `/usage`，所以同一个 Claude 账号 10 分钟（`claudeRefreshGap`）内只刷新一次，其他账号 1 分钟（`refreshGap`）内只刷新一次，按上次尝试计算，过早时返回 `ErrRefreshLimited`。网关对同一张额度卡 30 秒内只读一次，最多等 30 秒，请求的超时为 45 秒。引擎只在手动刷新凭证已过期的账号时调用，见下文“额度记录” |
| `Models` | 网关的 `GET /v1/models` |
| `OpenModelStream` / `ReadModelStream` / `CloseModelStream` | 向网关发送点火请求（`openai-response` 为 `POST /v1/responses`，`claude` 为 `POST /v1/messages`，`openai` 为 `POST /v1/chat/completions`），带 `X-Magpie-Account`；关闭流时取消请求 |
| `Log` | 标准错误输出，由插件宿主转交 Magpie 的日志 |

Magpie 的文件和响应格式没有公开承诺。格式与预期不符时，实现返回包装 `host.ErrFormat` 的错误，不猜测字段。以下格式按 Magpie 2a2a8d7 和 agy-cli 插件 0.1.106（magpie-community/plugins f4c3f89）核对。

### 网关请求

`internal/magpie` 直接连接 `MAGPIE_ADDR`（`0.0.0.0:<端口>` 和 `:<端口>` 改为 `127.0.0.1:<端口>`，与插件宿主相同），不经过环境变量中的代理。网关对来自本机回环地址的请求不要求密钥；带有转发头（`X-Forwarded-For`、`Forwarded`、`X-Real-IP`、`Cf-Connecting-IP` 等）或浏览器特征（`Origin`、`Sec-Fetch-*`、以 `Mozilla/` 开头的 User-Agent）的请求按其他机器的请求处理，需要网关密钥（Magpie 的 `internal/gateway/lan.go`），因此请求中不带这些头。`MAGPIE_ADDR` 设为局域网地址时，请求不算来自本机，网关会拒绝，Lamplighter 不处理这种情况。

所有网关请求共用 `internal/magpie/client.go` 中的 HTTP 客户端（不用代理，连接超时 10 秒，每个请求通过自己的 context 限时）和辅助函数：`gatewayURL` 拼出地址，`newGatewayRequest` 加上 User-Agent `magpie-lamplighter`，`gatewayJSON` 发送请求并解析 JSON 响应。连不上网关时得到 `Code` 为 `gateway_unreachable` 的 `host.Error`。网关返回非 2xx 时 `gatewayError` 给出 `host.Error`：`Code` 为 `gateway_http_error`，`Method` 为请求方法和路径，`Message` 为响应正文的前 4096 字节（按 UTF-8 字符截断），即客户端格式的错误对象：`/v1/messages` 为 `{"type":"error","error":{"type","message"}}`，其余为 `{"error":{"message","type","code","param"}}`（Magpie `writeErrorAs`）。`failure.Diagnose` 和 `ignite.Classify` 从中读取状态码和错误代码。

`magpie.Host` 自己保存运行中的状态：在内存中刷新的 Google token、每个账号上次调用 `RefreshQuota` 的时间和打开的模型流，都是 `Host` 的字段，不同的 `Host` 互不影响。

### 凭证

Magpie 用 pin 中的 id 指认账号：Codex 和 Claude 账号是 `logins.json` 中该条目的 `id`，没有 `id` 时是 `"magpie login\n<agent>\n<小写账号名>"` 的 SHA-256 前 8 字节；插件账号的 id 总是用插件服务名（如 `agy-cli`）和账号名这样算出（`nameID`）。

| 服务 | 凭证来源 |
| --- | --- |
| Codex（`codex`） | `logins.json` 中 `agent` 为 `codex` 的条目的 `auth`，即 Codex 的 `auth.json`：`tokens.access_token`、`refresh_token`、`account_id`（没有时取 `id_token` 中的 `chatgpt_account_id`）。access token 的 JWT `exp` 已过时设 `Expired` |
| Claude（`claude`） | `logins.json` 中 `agent` 为 `claude` 的条目的 `auth.claudeAiOauth`（`accessToken`、`refreshToken`、毫秒时间戳 `expiresAt`）。Claude Code 在账号自己的配置目录 `claude-accounts/<SHA-256(小写账号名) 前 16 个十六进制字符>/.credentials.json` 中刷新 token，Magpie 隔一段时间才写回 `logins.json`，所以那里的 `expiresAt` 更晚时用那里的 token（macOS 上 Claude Code 把它放在钥匙串中，这个文件可能不存在）。`expiresAt` 已过时设 `Expired`。用 setup-token 添加的账号（`auth.setupToken`）的 token 不能读额度，返回 `Expired`，由引擎改读额度记录 |
| Antigravity（`agy-cli`） | agy-cli 插件（`@magpie-community/opencode-agy-cli-auth`）的账号。`logins.json` 中 `agent` 为 `plugin:agy-cli` 的条目的 `home` 是该账号在 `plugin-auth.json` 中的键，键下 `metadata.agyHome` 指定 agy 的登录文件 `$XDG_STATE_HOME/magpie-agy-bridge/accounts/<agyHome>/.gemini/antigravity-cli/antigravity-oauth-token`（没有 `XDG_STATE_HOME` 时为 `~/.local/state`）。没有 `agyHome` 的账号是 agy 自己登录的第一个账号：macOS 和 Windows 上凭证在系统钥匙串中，Lamplighter 不读钥匙串（读取可能弹出授权框），返回 `errAgyKeyring`；Linux（如 Magpie 的 Docker 镜像）上读 `~/.gemini/antigravity-cli/antigravity-oauth-token`。文件内容是 `{"token": {"access_token", "refresh_token", "expiry"}, "auth_method", "id_token_email"}`，也可能带 go-keyring 的 `go-keyring-base64:` 前缀 |

Claude 和 Codex 的 token 刷新时会更换 refresh token，Lamplighter 只读取、绝不刷新，否则会让 Magpie 或代理 CLI 掉线。Google 刷新 token 时 refresh token 不变：Antigravity 的 access token 已过期或 2 分钟内过期时，用 agy 的 OAuth 客户端向 `https://oauth2.googleapis.com/token` 刷新（经该账号的代理），新 token 只保存在内存中直到过期前 2 分钟，不写回 agy 的文件。Google 返回 `invalid_grant` 时报告需要在 Magpie 中重新登录；没有 refresh token 且 access token 已过期时返回 `Expired`。Antigravity 的 Cloud Code project 是个人 Google 账号共用的 `aicode-consumers`（`auth_method` 为空或 `consumer`）；其他 `auth_method` 的账号需要另行查询 project，返回错误。

`Fingerprint` 是 refresh token 的 SHA-256 前 8 字节，没有 refresh token 时用 setup-token 或 access token，见“凭证问题”。`ProxyURL` 的取法见“网络出口”。

### 额度记录

`KeptQuotas` 把 `kept=1` 返回的每张 `kind` 为 `subscription` 的额度卡，按服务和账号名（`user`，不区分大小写）对应到 `Accounts` 中的 pin，转成与该服务 `Fetch` 相同的额度组和窗口 ID。来自远程 Magpie 的卡（`from` 不为空）和对应不上的卡不输出。读取时间取 `readAt`，没有时取 `asOf`，都没有时不输出。

| Magpie 的窗口名 | 额度组 key | 窗口 ID |
| --- | --- | --- |
| Codex 的 `5 hours`、`7 days` | `codex:main` | `five-hour`、`seven-day` |
| Codex 的其他 `<n> hours`、`<n> days` | `codex:main` | `window-<长度>`，如 `window-30d` |
| Claude 的 `5 hours`、`7 days` | `claude:main` | `five-hour`、`seven-day` |
| Claude 的 `7 days · Opus`、`7 days · Sonnet` | `claude:seven-day-opus`、`claude:seven-day-sonnet` | `seven-day-opus`、`seven-day-sonnet` |
| Claude 的 `7 days · Fable` | `claude:seven-day-fable` | `seven-day-fable` |

剩余额度为 `100 - used`。文字行（`text`）、无限额度（`unlimited`）、其他通道（`lane`）、钱包余额（`balance`）和其他名称的窗口（如 `Allowance`、`Extra usage`）不输出。Magpie 为还没有开始 5 小时窗口的 Claude 账号补一个没有重置时间、用量为 0 的 `5 hours` 窗口，额度接口不返回这样的窗口，所以没有重置时间且用量为 0 的窗口也不输出。参与转换的窗口缺少 `used` 时返回 `host.ErrFormat`。

Antigravity 不输出额度记录：Magpie 按额度池命名它的窗口（`Gemini · 5 hours`），不带 `Fetch` 用作窗口 ID 的 `bucketId`，转换后不会更新任何已有窗口。Antigravity 的 token 可以在内存中刷新，主动查询总能进行，所以不需要这份读数。

被动数据：引擎每 60 秒（`keptInterval`）读一次 `KeptQuotas`，在本轮主动查询之后应用每个账号比上次更新的读数（`pollKept`）。Magpie 根据它转发的请求更新这些读数，Lamplighter 因此不需要逐请求的响应头。

**过期的凭证**：`Credential` 返回 `Expired` 时，引擎不向厂商查询，改用该账号在额度记录中的读数（`keptFallback`）：比已应用的读数新时应用，没有读数时报告错误。手动刷新（页面的刷新按钮、`POST /api/refresh` 和 MCP 的 `refresh_quotas`，包括只刷新一个账号）时，引擎先调用 `RefreshQuota` 让 Magpie 重新读取这个账号的额度，再读一次额度记录（`refreshKept`）；`RefreshQuota` 失败时照样应用已有的读数，并在账号的错误中写明刷新失败的原因。一次手动刷新中的所有 `RefreshQuota` 共用 35 秒的期限（`manualRefreshBudget`）：经 Magpie 调用的 tool 最多等约 50 秒，网关等一张额度卡最多 30 秒。定时查询和重置前后的查询不调用 `RefreshQuota`，因为 Claude 的刷新会运行 Claude Code 的 `/usage`。

## HTTP 接口

Lamplighter 有两个监听，使用同一套路由（`server.Server.routes`）：

- 内部监听：`-listen` 的地址，只绑定回环地址，不要求密钥。Magpie 的页面服务（`magpie.page.service`）和插件宿主的 tool 经它访问。路由外面另有 Host 检查、页面令牌和 CORS，见下文“内部监听”。
- 对外监听：配置中的 `public_listen`（默认 `127.0.0.1:8318`），供服务器前面的域名或反向代理使用，页面和接口要求 `admin_key`，见下文“对外监听”。

路由：

| 方法和路径 | 作用 |
| --- | --- |
| `GET /` | 管理页面 |
| `GET /api/status` | 状态页数据，配置中的密钥只显示是否已设置（`admin_key_set`、`mcp_key_set`）；`public` 和 `mcp` 是对外监听的状态，见下文“对外监听”；`magpie_warmup` 是 Lamplighter 会点火、Magpie 也开启了预热的账号 pin 列表，见“Magpie 的预热”。`providers` 按显示顺序列出支持的服务（`id`、`title`、能否点火 `ignite`、中英文的模型提示 `model_hint`、可勾选的点火额度组 `groups` 及说明 `groups_hint`），`group_order` 是额度组的显示顺序；每个窗口带 `kind`（`5h` 或 `7d`，其他长度的窗口没有这个字段） |
| `GET /api/history?range=…&window=5h\|7d` | 额度历史和点火事件，参数和返回格式见下文“额度变化图表” |
| `POST /api/refresh` | 立即主动查询，`{"auth_index": "<pin>"}` 只查一个账号；凭证已过期的账号先让 Magpie 刷新额度，见“与 Magpie 的接口”的“额度记录” |
| `POST /api/ignite` | 立即点火，`{"target": "<额度组 key>"}` |
| `POST /api/test-notify` | 同时向已配置的渠道发送测试通知，`results` 中每个渠道一项：`channel`、`ok`、`error`，webhook 还有 `response`（响应的前 200 字）和 `unchecked`（没有设置 `success_json`，只检查了 HTTP 状态码） |
| `POST /api/language` | 记录通知语言，`{"language": "zh-CN"}`；中文的各种写法记为 `zh`，其他语言记为 `en` |
| `POST /api/check-update` | 检查新版本，返回状态；页面打开时调用 |
| `GET /api/config` | 已保存的配置原文，含密钥，供设置页填表 |
| `PATCH /api/config` | 替换给出的顶层键，值为 `null` 时删除该键；合并结果必须能解析，然后整体写入 `config.json` 并应用。返回保存后的原文 |
| `POST /api/tools/{name}` | 执行一个 MCP 工具，请求体为参数对象，返回 `{"result": …}` 或 `{"error": "…"}`；插件宿主的 tool 经这里调用，最长 50 秒 |

`account_names` 以 pin 为键保存账号名称：Magpie 没有让插件修改账号名称的接口。

### 内部监听

内部监听的处理器是 `server.guardInternal` 包着的路由，代码在 `internal/server/internal.go`。

**Host 检查**：请求的 Host 不是 `localhost` 或回环 IP（`127.0.0.0/8`、`::1`，端口不限）时返回 403，包括 `GET /`、`/api/*` 和预检请求。内部监听不要求密钥，恶意域名把自己的名字重新解析到 `127.0.0.1` 后，在浏览器中与内部监听同源，能读到页面和 `GET /api/config` 中的密钥；这类请求的 Host 是那个域名。合法的访问都用回环地址：Magpie 的页面服务代理把 Host 设为 `127.0.0.1:<端口>`（Magpie 源码 `internal/gui/pageservice.go` 的 `servePageService`），桌面窗口中的页面直接打开 `http://127.0.0.1:<端口>/`，插件宿主经 `127.0.0.1` 调用 tool。所以把 `-listen` 绑定到其他地址后从别的机器访问，也会得到 403。

**页面令牌**：`server.New` 用 `crypto/rand` 生成 32 字节随机数，以 base64url（无填充）编码作为页面令牌，进程运行期间不变。内部监听的 `GET /` 返回 `web.PageWith(令牌)`：它把页面中的 `<meta name="lamplighter-page" content="">` 填上令牌，页面没有这个标签时原样返回。页面请求接口时在 `X-Lamplighter-Page` 请求头中带上令牌。对外监听返回不带令牌的 `web.Page()`。

**CORS**：Magpie 的插件页面在 `sandbox="allow-scripts"` 的 iframe 中运行，页面服务代理还加上 CSP `sandbox`，页面是不透明源，请求自己的 `api/…` 时带 `Origin: null`，没有 CORS 头就读不到响应。任何网站都能建这样的 iframe，所以只对带页面令牌的请求开放：

- `/api/*` 的 `OPTIONS` 预检且 `Origin` 为 `null`：返回 204，带 `Access-Control-Allow-Origin: null`、`Access-Control-Allow-Methods: GET, POST, PATCH`、`Access-Control-Allow-Headers: Content-Type, Authorization, X-Lamplighter-Page`、`Access-Control-Max-Age: 600`、`Vary: Origin`。预检本身不带自定义请求头，所以不检查令牌。
- `/api/*` 的其他请求：只有 `Origin` 为 `null` 且 `X-Lamplighter-Page` 等于令牌时（两边先求 SHA-256 再常数时间比较），才加 `Access-Control-Allow-Origin: null` 和 `Vary: Origin`。其余请求不加 CORS 头，但照常处理：没有 `Origin` 的请求（插件宿主调用 `POST /api/tools/{name}`）和同源请求不受影响。别的网站在 sandbox iframe 中仍能发出请求，只是读不到响应；令牌只在内部监听返回的页面里，它拿不到。
- 不返回 `Access-Control-Allow-Credentials`，`GET /` 不加 CORS 头。

对外监听不加 CORS：独立访问时页面与接口同源，鉴权靠 `admin_key`。

### 对外监听

对外监听由 `mcp.Server` 持有（自己的 `net.Listen` 和 `http.Server`）。`server.New` 用 `mcp.Server.SetWeb` 把上述路由交给它，`server.Server.apply` 每次应用配置时调用 `mcp.Server.Configure`。监听上提供两部分，各自按配置开启：

| 部分 | 开启条件 | 鉴权 |
| --- | --- | --- |
| 页面和接口（`/mcp` 以外的路径） | `web_enabled` 为真且 `admin_key` 非空 | `Authorization: Bearer <admin_key>` |
| `/mcp`，见“MCP” | `mcp_enabled` 为真且 `mcp_key` 非空 | `Authorization: Bearer <mcp_key>`，以及 Origin 校验 |

- 两部分都没有开启时不监听，端口释放。`admin_key` 不校验格式，非空即可；设置页生成的密钥为 `lladm_` 加 32 字节随机数的 base64url 编码。
- `GET /`（以及 `HEAD /`）不要求密钥：页面本身不含数据，浏览器打开页面时也带不上请求头。页面请求接口收到 401 后让用户输入管理密钥，见“管理页面”。其他路径（包括不存在的路径）缺少密钥或密钥错误时返回 401 和 `WWW-Authenticate: Bearer`，不暴露有哪些路由；带对密钥后，不存在的路由返回 404。页面和接口没有开启时，`/mcp` 以外的路径都返回 404；`/mcp` 没有开启时返回 404。
- 两把密钥只开放各自的部分：`admin_key` 不能访问 `/mcp`，`mcp_key` 不能访问页面和接口。比较方法相同：对 `Bearer <密钥>` 和请求的 `Authorization` 头各求 SHA-256，再常数时间比较，不泄露密钥长度和前缀。
- 页面和接口不做 Origin 校验，也不返回 CORS 头：密钥在请求头中，其他网站的页面无法让浏览器带上它。
- **重新绑定**：只有地址变化，或监听从有内容可提供变为没有（反之亦然）时，`Configure` 才关闭旧监听再绑定新地址。监听在运行且地址不变时，更换密钥或开关其中一部分只替换内存中的设置，监听保持不动：反向代理和 MCP 客户端会复用 keep-alive 连接，关闭中的监听会让下一个请求失败。请求在 `mcp.Server.mu` 内读取密钥，`Configure` 返回后的请求都按新设置处理。用旧 `mcp_key` 进入的 MCP 请求，以及关闭 `/mcp` 时仍在执行的 MCP 请求会被取消；页面和接口的请求执行到结束，所以经对外地址修改 `admin_key` 的 `PATCH /api/config` 也能收到响应。绑定失败时记录错误，每 2 秒重试。
- **关闭**：`server.Server.Shutdown` 先在 `server.Server.mu` 内标记停止，然后释放这把锁，再关闭对外监听（取消并等待其中的请求），最后停止引擎。对外的接口请求读取状态时要取这把锁，持锁等待会死锁。停止后的实例不再应用配置，也不重新监听。
- **超时**：读请求头 5 秒、读请求 15 秒、写响应 190 秒、空闲连接 30 秒，请求头最多 16 KiB，页面、接口和 `/mcp` 共用。插件宿主的 tool 调用走内部监听，经对外接口调用 `POST /api/tools/{name}` 同样最长 50 秒。

状态接口中的对外监听状态：

- `public`：`listen`（`public_listen`）、`running`（正在监听）、`error`（绑定或运行的错误，没有时为空字符串）、`web`（页面和接口正在提供）、`mcp`（`/mcp` 正在提供）。两部分都没有开启时 `running`、`web`、`mcp` 都为假。
- `mcp`：`enabled`（`mcp_enabled`）、`listen`、`running`（监听正在运行）、`error`、`path`（固定为 `/mcp`）。`error` 在开启 MCP 但 `mcp_key` 为空时为 `MCP key is empty`，开启 MCP 时为监听的错误，关闭 MCP 时为空。
- `status.config` 不含 `admin_key` 和 `mcp_key`，以 `admin_key_set`、`mcp_key_set` 表示是否已设置；`GET /api/config` 返回原值供设置页填表。

## 管理页面

### 运行环境

页面由内部监听的 `GET /` 提供，也可以经对外监听访问。在 Magpie 中，插件页面的 iframe 先加载插件包中的页面文件，再跳到内部监听：桌面窗口直接访问 `127.0.0.1:<端口>`，`magpie web` 经 Magpie 页面服务的反向代理访问。Magpie 每次 Restart（安装、更新或设置插件，更新 Bun）都会结束插件宿主及其子进程；打开页面时 Magpie 先启动宿主，最多等 7 秒，服务未就绪时显示 502 页并每 2 秒重试。

iframe 设置了 `sandbox allow-scripts`，页面服务的响应也带同样的 CSP sandbox，所以页面在不透明源中运行（`window.origin` 为 `"null"`，`page.js` 中的 `SANDBOXED`）：没有 `localStorage`，`confirm()` 不弹窗直接返回假，不能写剪贴板，表单不能提交。页面按以下方式处理：

- `localStorage` 的每次读写都放在 `try` 中。沙箱中读写失败时，视图、打码、图表筛选、5 小时重置开关、MCP 对外地址和管理密钥只在本次打开期间有效。
- 需要确认的操作（立即点火、替换管理密钥或 MCP 密钥）经 `confirmed` 询问：沙箱外调用 `confirm()`；沙箱中在提示框里显示确认文字，5 秒内再点一次同一个按钮才执行。
- 复制文字时，沙箱中不调用 Clipboard API，改用选中文本加 `execCommand("copy")`；失败时提示手动复制，或经对外地址在浏览器中打开页面。截图见下文。
- 管理密钥输入框的按钮为 `type="button"`，点击按钮和在输入框中按回车都提交密钥，不依赖表单的提交事件。

页面在沙箱中请求自己的 `api/…` 也是跨源请求（`Origin: null`）。内部监听把进程启动时生成的令牌写入页面的 `<meta name="lamplighter-page" content="">`，页面读取它（`pageToken`），非空时在每个请求中以 `X-Lamplighter-Page` 发送；内部监听只对带正确令牌的 `Origin: null` 请求返回 CORS 头，见“HTTP 接口”。其他网站读不到页面，也就拿不到令牌。页面的请求只由自己的代码发出（`api`），不按父窗口的消息发请求或返回数据，嵌入页面的网站无法借用令牌。

### 与 Magpie 窗口的通信

页面不加载 Magpie 的 `bridge.js`，自己实现其中的消息协议（Magpie 源码 `internal/gui/pagebridge.js`，窗口一侧为 `internal/gui/assets/pluginpages.js`）。Magpie 按 `ev.source` 识别插件页面的 iframe，与其中加载的是哪个文档无关，所以跳到内部监听之后仍能通信。

- 页面在 iframe 中（`window.parent !== window`）加载时向父窗口发送 `{magpie: 1, id, method: "hello"}`（`magpieHello`），Magpie 答复 `{dark, lang, tokens, context, service, visible}`。答复到达后 `magpie.ready` 为真。
- Magpie 在主题或语言变化时发送 `{magpie: 1, event: "theme", data: {dark, lang, tokens}}`，在读者离开和回到页面时发送 `{magpie: 1, event: "visibility", data: {visible}}`。页面只处理来自 `window.parent`、带 `magpie: 1` 的消息（`magpieMessage`）。
- 离开页面期间跳过每 30 秒一次的状态刷新，回到页面时立即刷新。
- 页面不发送 `error` 方法：Magpie 收到后会用“页面出错”替换整个插件页面。
- 页面单独打开或 Magpie 不答复时，主题和语言跟随浏览器。

### 独立访问和管理密钥

经对外监听打开时，`api` 收到 401 后显示“需要管理密钥”。输入的密钥保存在 `localStorage` 的 `lamplighter-admin-key` 中，之后以 `Authorization: Bearer <密钥>` 发送。请求没有带密钥时只显示输入框；带了密钥时清除它并显示“管理密钥无效”。请求发出后页面换了密钥（例如本页刚生成了新的管理密钥）时，迟到的 401 不清除新密钥。本页生成新的管理密钥后，带密钥访问的页面改用新密钥。

### 页面功能

设置通过 `PATCH /api/config` 自动保存：开关和勾选框的 `change` 事件触发保存，文本和数字在失焦或按回车时保存，每次只提交改动字段所在的顶层键；三档提醒阈值校验 `notice ≥ low ≥ critical` 后一起提交。`bark_url` 去掉尾部斜线，空的对外监听地址按后端默认值保存，避免已应用判断与后端规整结果不一致。该接口整体替换顶层键，所以 `ignition`、`providers`、`recovery_notify` 等对象以已保存的对象为基础整体提交。保存依次进行，后一次以前一次写入的配置为基础；数字不满足输入框的范围或步长时不保存。页面的请求都使用相对路径（`api/…`），所以页面在自己监听的根路径和 Magpie 页面服务的反向代理后面都能工作。

浏览器会把为本站保存的密码（通常是管理密钥）填进密码输入框。输入框没有焦点时由浏览器填入（匹配 `:autofill` 或 `:-webkit-autofill`）的值来自页面加载时的自动填入，不保存也不提示，输入框恢复为已保存的值。`autocomplete="off"` 对密码框不起作用，`new-password` 会让 Chrome 提示生成强密码，所以都不使用。

`webhook.headers` 和 `webhook.success_json` 在页面上以文本编辑（每行一个 `名称: 值`、JSON 对象），无法解析时不保存。状态接口只返回 webhook 的地址、请求头和请求体是否已设置，页面从 `GET /api/config` 读取原值填入表单。

保存失败时保留输入草稿；普通设置通过 `settingsDirty`、账号字段通过 `accountDirty` 避免回填覆盖草稿。`captureIntent` 在变更事件发生时保存字段值及各控件的 `settingsEdit` 编辑代次，队列中的 `buildPatch` 按这份意图构造补丁并记录实际提交的控件。配置确认后，`refillSaved` 清理本次实际提交、代次仍匹配且没有独立失败标记的控件草稿，再回填有效值。同一顶层键内其他控件的后续保存不阻止这项清理；后续编辑的草稿、无法解析或未提交的字段仍保留。单选组共享编辑代次并作为整体确认；组内仍有草稿或失败时跳过整组回填，避免设置某个单选框时间接覆盖较新的选择。`claimState` 按项设置 `owner`，后续状态通过绑定该所有权的 `say` 更新。任何 `input` 事件都会推进编辑代次、撤销旧 `owner` 并清除该项的普通进度，未解决的失败仍保留。旧请求继续完成并等待自己的令牌，但不能用迟到的响应更新新输入的结果。

每次保存携带新的 `config_token`，它是可为空的字符串，不是密钥，不参与配置校验和运行时行为。`status.config` 表示最近接收的有效配置；顶层 `config_applied_token` 表示引擎后台循环已经确认并完成运行时应用、且插件已执行完 `mcp.Configure` 的同一份配置。只修改 MCP 或其他非运行时字段也需要后台循环确认；MCP 监听成功、停用或绑定失败并记录 `mcp.error` 均算完成配置执行，监听是否可用仍读取 `mcp.running` 和 `mcp.error`。解析失败的配置不保存，保留上一令牌；程序重启后从 `config.json` 读回同一令牌，无需持久化计数器。插件以实例内接收序号协调两项确认，读取管理状态时持有生命周期锁，避免跨配置拼接状态。页面每次 PATCH 插件配置都用 `crypto.getRandomValues` 生成 8 字节随机数，以 16 位十六进制字符串作为令牌；设置、权重和卡片顺序等写入共用 `patchConfig`。保存后显示“已保存，等待生效”，等待期间轮询状态；轮询在前一次请求结束后间隔 1 秒发起，单次请求通过 `AbortController` 设置 4 秒超时。`config_applied_token` 与本次令牌相等，或等于本页之后一次保存的令牌时，本次保存显示“已生效”；`whenApplied` 为每次等待设置独立的 15 秒截止计时器，状态请求挂起也会按时结束等待、移除等待项并显示“未确认生效”；截止后到达的令牌不改变已结束的等待结果。脱敏占位和 `mcp_key_set` 不用于确认密钥替换。MCP 配置确认后继续结合 `status.mcp.running` 和 `status.mcp.error` 显示服务状态及错误。测试推送先等待未完成的保存，再按相同令牌规则最多等待 15 秒；确认后发送，超时则显示“配置尚未生效，未发送测试”，不调用测试接口。

“设置”视图“账号”区的“名称”输入框把整个 `account_names` 对象（以 pin 为键）通过配置接口保存，留空则删除这个账号的名称，账号重新显示 Magpie 中的名称；然后调用 `refresh` 查询这个账号，使状态立即带上新名称。额度查询失败不影响改名，只是不更新页面，所以 `refresh` 的结果不作为改名的结果显示。同一服务有多个账号时，该区还显示“权重”输入框：输入大于 0 的数字后，页面把整个 `account_weights` 对象保存，留空则删除这个账号的权重，并立即按新值重画；`config.Parse` 丢弃不大于 0 或无穷大的权重。名称和权重在离开输入框或按回车时通过保存队列提交。名称、权重及排序分别持有保存结果和提示所有权，在“账号”标题右侧汇总显示；字段保存失败时保留输入草稿。账号行按 pin 复用 DOM，刷新、排序和较早保存的响应保留正在编辑的输入及焦点。保存结果按稳定的账号字段键保留，与当前 DOM 绑定分开；监控关闭再开启导致账号行重建时，`saveEntry` 重新绑定控件并恢复失败标记、草稿和待保存计数。排队中的账号保存通过当前绑定完成更新。

页顶、卡片区标题栏和曲线详情标题栏的眼睛按钮共用一个打码开关，状态保存在 `localStorage` 的 `lamplighter-mask-accounts` 中（`1` 为打码），脚本加载时即应用到根元素，首次渲染前生效；各浏览器分别保存。HTML 中的账号标识及“账号”区的输入框使用 `.account-sensitive` 和 CSS `blur`，SVG 图表文字使用模糊滤镜；敏感标识集合同时读取当前账号、事件标签和历史系列标签，包含改名及删除后仍保留的名称。当前状态没有对应额度组元数据的历史系列将 `label`、`source_label` 和 `group` 全部纳入敏感集合，即使标签和来源标签相等。没有当前账号的行标记为 `orphan`，在概览 SVG、详情标题和数据表中将名称整体模糊，读屏标签使用账号编号；详情截图保留相同模糊状态。事件标签整体标为敏感，详情中的标签、错误摘要（`params.summary`，没有时为页面提炼的一行，见下文“事件列表”）和错误原文（`params.error`）也整体标为敏感。以下事件的详情整段标为敏感：`error` 事件；事件本身没有 `params`、也没有存储的 `label` 的旧事件（不看由 `group` 推出的标签）；没有 `params` 且为失败或警告样式的旧事件（`ignite_failed`、`ignite_paused`、`notify_failed`、`error`、`model_refused`、`cooldown_stale`、`plan_change`、`exhausted`，以及未知类型中级别为 error 或 warn 的），因为这些原文可能提到已删除或改名的账号，按当前账号名替换遮不住。旧的成功类事件和带 `params` 的事件仍只遮住账号名和错误原文。概览及详情的点火悬停提示沿用相同规则。打码时省略含账号名的 `title` 提示，以及事件行的推送标记和事件详情的悬停提示，无障碍标签使用账号序号；清空名称等确认对话框保留原文。此开关不改变接口数据或 DOM 中的原始文本。

账号卡片的头部只显示服务标签（含非默认权重倍数）和邮箱。底部的 `.account-head.cell-note` 左侧显示套餐，右侧依次放置“刷新”和相机按钮；按钮使用 `.no-snapshot`，不出现在导出的图片中。底部行在卡片拉伸时贴齐内容区底边，套餐名称过长时单行省略。有套餐的卡片保留头部及原套餐行占用的高度，无套餐或无额度内容的卡片增加相同样式的底部操作行。卡片区标题栏的操作位置独立于卡片内部布局。

卡片、卡片区和曲线详情图的相机按钮调用 `snapshotPng`，克隆目标节点并移除 `.no-snapshot` 操作按钮，将当前样式、主题和模糊状态通过 SVG `foreignObject` 绘制到 canvas，按设备像素比生成 PNG，再通过 `ClipboardItem` 写入剪贴板。曲线详情图的截图对象是 `#chart-detail-wrap`，标题行和图表在这个容器里。截图外框的内边距、背景及静态样式统一定义在 `page.css` 的 `.snapshot-frame` 规则中，离屏测量和 SVG 导出使用同一份样式。过程不加载页面外资源，避免 canvas 被跨源内容污染；动画、过渡和悬停样式不参与截图。在 Magpie 中（`magpie.ready`），PNG 交给 Magpie 的 `image.copy`，由 Magpie 写入系统剪贴板，在 `magpie web` 中由浏览器窗口复制。没有 Magpie 时通过 `ClipboardItem` 写入剪贴板，只在沙箱外的安全上下文中、浏览器支持图片剪贴板时启用按钮，复制仍受浏览器权限限制。

顶部的“额度 | 设置”切换两个独立视图，所选视图保存在浏览器的 `localStorage` 中（`lamplighter-view`）。“设置”视图依次显示账号、通知渠道、提醒规则、自动点火、服务和高级。

`saveHost` 将普通设置的保存结果定位到 `.settings-section > .card-title`，高级设置定位到各自 `fieldset > legend`，每处只包含一个 `.save-state`。标题中的文字用子元素承载，切换语言时保留状态节点，`saveText` 按当前语言显示本地状态文案，上游错误原文保持不变。每个标题通过 `saveEntries` 按项保存状态、控件和独立所有权；没有失败时按保存发起顺序显示最近一次进度，较早保存的迟到确认不能覆盖较新的进度。失败独立保留到该项重新写入成功，其他项成功和输入事件不能清除；多项失败时显示最近的失败原因与其余数量。失败控件带 `aria-invalid="true"`，边框使用 `--error-color`；单选框和勾选框使用同色外框。标题及 legend 保持固定行高，失败摘要、原因、计数通过 `data-save-part` 分开渲染。桌面端摘要及原因单行省略，失败数量单独保留；宽度不超过 640 px 时隐藏字段摘要与原因，只显示“保存失败”和失败数量，正常进度仍可单行显示。标题文字可以省略，为失败标识及数量保留空间。记住筛选条件的开关照常写入配置，在“额度图表”legend 显示保存结果，具体筛选值保存在浏览器中。测试推送的结果仍由按钮旁的 `#test-results` 显示；复制操作通过 toast 提示结果。

“设置”视图的“账号”区通过手柄支持一维拖动排序，以账号 pin 列表整体保存 `account_order`；页面在后端排序基础上按最新保存的列表重排，使卡片和图表立即响应顺序变化。拖动使用 Pointer Events，手柄设置 `touch-action: none`；固定定位的不可交互副本跟随指针，副本使用卡片的圆角、边框和背景并保留浮动阴影，原行隐藏内容并显示同样圆角的虚线边框作为占位。命中判断使用各行不含变换的布局中线，其他行以 180 毫秒的 FLIP 动画让位。松手时副本滑至占位行后保存，保存状态显示在“账号”标题旁；按 Esc 或收到 `pointercancel` 时恢复原顺序，不保存。拖动及落定期间推迟账号重绘，系统要求减少动态效果时跳过动画。

“高级”中的“对外访问”设置 `public_listen`、`web_enabled` 和 `admin_key`，状态读取 `status.public`（`listen`、`running`、`error`、`web`、`mcp`，`publicState`）；状态接口没有这个字段时退回读取 `status.mcp` 的 `listen`、`running` 和 `error`。开启 `web_enabled` 时没有管理密钥，页面用 `crypto.getRandomValues` 生成 32 字节随机数，以 `lladm_` 加 base64url 编码与开关一起保存；“生成新密钥”确认后替换它。“MCP”设置 `mcp_enabled` 和 `mcp_key`，监听状态读取 `status.mcp`；开启时没有有效密钥，同样生成 `llmcp_` 开头的密钥一起保存。两种密钥打码显示，可以显示和复制。保存这些字段后，对外监听的错误（`status.public.error`，没有 `status.public` 时为 `status.mcp.error`）显示为保存失败，页顶也显示提示。客户端配置中的地址取浏览器中保存的对外地址，没有时取对外监听地址。

`status.magpie_warmup` 列出 Lamplighter 会点火、Magpie 也开启了预热的账号 pin（见“Magpie 的预热”），字段缺失时按空数组处理。列表非空时页顶提示这些账号需要在 Magpie 中关闭预热，以免与自动点火重复点火。账号按页面上的名称显示（`warmupNames`）：服务名加名称（`note`）或邮箱；状态中没有这个账号时显示 `account_names` 中的名称，再没有时显示 pin。

页面标题旁的图标从 GitHub 加载仓库中的 `assets/logo.png`，更换图标不需要发布新版本。`raw.githubusercontent.com` 无法访问（例如在中国大陆）或 5 秒内没有加载完时，改经 `ghfast.top` 镜像加载；仍然失败时不显示。

图表的筛选按钮（`.segmented`）和设置中的三选一开关（`.mode-switch`）共用一套滑块：`page.js` 的 `placeThumb` 测量选中项的位置和尺寸（取不含缩放的小数值），写入滑块的 CSS 变量。各选项宽度不同，所以按测量值定位。用户点选时滑块带回弹滑过去并挤压一下；其他情况直接定位：代码改变选中项时（如首次填入设置、恢复筛选条件）直接调用 `placeThumb`，切换语言、窗口尺寸变化和从隐藏变为可见时由监听控件及其选项尺寸的 `ResizeObserver` 调用。时间范围按钮只在切换额度窗口或语言时重建，点选时原地更新 `aria-pressed`，滑块才能滑动。点击已选中的按钮不改变选择；图表读取失败时，这样的点击会重新读取。系统设置减少动态效果时，`prefers-reduced-motion` 规则关闭这些动画；该规则放在 `page.css` 末尾，以覆盖前面定义的过渡。

页面文字都在 `page.js` 的 `I18N` 词典中，中文和英文各一份；`page.html` 中的静态文字用 `data-i18n`（以及 `data-i18n-placeholder`、`data-i18n-title`、`data-i18n-aria-label`）标出词条，其中的中文原文只在脚本应用语言前显示。语言跟随 Magpie（hello 答复和 `theme` 事件中的 `lang`，如 `zh`、`zh-TW`、`en`），单独打开时跟随浏览器语言（`navigator.languages[0]`）并监听 `languagechange`；`zh` 开头的显示中文，其余显示英文（`pageLanguage`）。状态接口的 `language` 与页面语言不同时，页面调用 `POST .../language` 上报，失败时在下一次刷新状态时重试。

事件文字由页面按事件的 `params` 用当前语言拼出（见下文“数据”）。没有 `params` 的旧事件显示存下的中文原文。上游返回的错误原文和推送标题不翻译：推送标题本来就是按通知语言生成的。

事件列表的规则：

- **推送标记**：`notify` 和 `notify_failed` 事件按 `params.notify_kind` 挂到触发它的事件行尾，显示“已推送”或“推送失败”，悬停显示推送标题。对应关系（`NOTIFY_TRIGGERS`）：`recovery` 对应 `reset`，`circuit` 对应 `ignite_paused`，`cooldown` 对应 `cooldown_stale`，`plan` 对应 `plan_change`，`credential`、`suspended`、`rate_limited` 对应 `credential_invalid`，`credential_ok` 对应 `credential_restored`，`codex_reset` 对应 `codex_reset`。推送与触发事件的时间差有范围限制（`NOTIFY_LAG`）：`codex_reset` 以外的种类为触发事件之后 70 分钟内，因为发送失败的推送会随之后的轮询重试最长 1 小时；`codex_reset` 为前后 5 分钟内，因为 Did Codex Reset 先发送推送，再记录 `codex_reset` 事件。触发事件还须属于同一额度组：两边都有 `group` 时比较 `group`，否则比较标签；`codex_reset` 不属于任何账号，要求两边都没有 `group`。有多个候选时挂到时间最近的一条。`recovery` 带 `windows` 时按窗口 ID 匹配，每个窗口挂最近的一条；不带时挂最近的一条及其 60 秒内的同组重置。候选是已并入凭证问题行的 `ignite_paused` 时（见“凭证问题”），推送挂在那一行。`quota`、`reminder` 和找不到触发事件的推送单独成行；单独成行的 `codex_reset` 推送本身没有标签，标签列显示 `Did Codex Reset`。没有 `notify_kind` 的旧事件按标题推断种类（`notifyKind`）。
- **5 小时重置**：`reset` 事件中 5 小时窗口的默认隐藏（连同挂在上面的推送），开关状态保存在 `localStorage` 的 `lamplighter-events-5h` 中。窗口种类取 `params.kind`，旧事件按窗口 ID 推断。耗尽事件总是显示。
- **错误**：列表中的错误只显示一行：优先用 `params.summary`，否则由 `errorSummary` 提炼（JSON 之前的文字，加上 JSON 中的 HTTP 状态码 · reason · message；解析不出时取第一行），超过 160 个字符（`ERROR_CHARS`）截断；完整原文在悬停提示中。手动点火失败（`manual` 为 `1`）只显示摘要，不显示重试时间。`ignite_paused` 没有 `until` 时，`reason` 为 `auth` 或 `suspended` 显示“暂停，等待更新凭证”或“暂停，账号已停用”加摘要，其余只显示摘要。
- **凭证问题**：`credential_invalid` 按 `reason` 显示凭证失效、账号停用、被限流三种徽标。同一额度组前后 60 秒内（`PAUSE_PAIR`）的 `ignite_paused` 并入时间最近的 `credential_invalid` 那一行，不单独显示。`source` 为 `ignite` 或并入了暂停时，详情是摘要后加“· 点火已暂停”，有 `until`（取凭证事件的，没有时取并入的暂停的）时改为“· 点火暂停到 时间”；其余情况下，限流且有 `until` 时加“· 限流到 时间”。悬停提示每项一行，依次为“HTTP 状态码 · code · summary”（缺少的部分省略）、`appeal_url`（有才写）、并入的暂停事件的 `params.error` 原文（有才写）；查询发现、没有并入暂停的 `credential_invalid` 只有前两项。打码时不显示悬停提示。`appeal_url` 是 http 或 https 地址时显示申诉链接。

页面的 CSS 变量沿用 CPA 管理中心 `src/styles/themes.scss` 的名称和取值（浅色、深色两套），额度条和图表通过 `quotaLevel` 共用满、高、中、低、空五档颜色：原始值接近 100% 为满、接近 0% 为空（`LEVEL_EPSILON` 为 `1e-6`，容纳加权平均的浮点误差）；中间按前两档提醒阈值划分（默认 >50% 为高，>20% 为中，其余为低），满额和耗尽有独立颜色。判断先于显示值取整，因此 99.6% 不使用满额颜色；中、低的分界与通知标题的 🟡、🔴 一致，遥测数字使用等宽字体。主题跟随 Magpie 的 `dark`，单独打开时跟随浏览器的 `prefers-color-scheme` 并监听其变化（`pageTheme`）；深色时根元素带 `data-theme="dark"`。Magpie 的主题只分浅色和深色，页面使用自己的配色，不使用 Magpie 传来的 `tokens`。

额度变化图表的数据处理都在页面中完成：

- **时间轴**：1、3 小时和 1、4 天的横轴右端是现在。从 6 小时、8 天起（5 小时额度的 6、12、24、26 小时，7 天额度的 8、15 天、1 个月和 36 天），横轴总长仍等于所选范围，右端是所选行当前窗口的重置时间。合计行取各账号中最早的下一次重置，即行右侧 ↻ 的时间；已经过去的重置时间（窗口还没有重新开启，或之后没有查询到）不算。例如 6 小时显示当前 5 小时窗口和上个窗口的最后 1 小时。`RANGES` 中的 `24h` 既是 5 小时额度的 24 小时，也是 7 天额度的 1 天，只有前者延伸（`extendsAhead`）。历史接口返回到现在为止的范围，页面不画横轴左端之前的部分。切换所选行时整张图平移；时间流逝时横轴不动，窗口重置后右端移到新窗口的重置时间。所选行没有重置时间、重置时间已过（5 小时窗口还没有被新请求开启），或重置时间离现在超过所选范围时，右端是现在。现在之后的部分画成与缺数据相同的斜纹，不画表示现在的竖线。详情图绘图区的左右两端是实线，与 0% 线相同；刻度离两端不到 4 像素时只标时间，不画刻度虚线，以免与边线并排。概览图和详情图共用这条横轴。
- **缺数据**：相邻样本间隔超过 3.5 个查询间隔（或接口的分段长度）时视为缺数据，画成斜线纹理，不延续前一个值。启用被动跳过时，阈值至少为 `passive_skip_max_minutes` 加 1.5 个查询间隔：响应头里没有的窗口（如没在使用的 Fable）在跳过期间只能等下一次主动查询，这段间隔不算缺数据。
- **多账号合计**：同一服务、同一 `source_label` 的多个账号合成一行，取各账号剩余百分比按权重的加权平均值（`pooled`），权重与各账号的额度大小成比例时，这等于总额度的剩余比例。合计行的当前值、曲线和延长线都用加权值；详情图的阴影仍是各账号的最低到最高。权重依次取 `account_weights` 中按账号 pin（状态接口中账号的 `name`，与 `auth_index` 相同）设置的值、状态接口中账号的 `default_weight`，都没有时为 1（`accountWeight`）；权重全为 1 时就是简单平均。
- **窗口种类与行集合**：标签页和图表行取当前状态中的窗口与所选范围内历史系列的并集，按 `kind` 分到“5 小时额度”和“7 天额度”两个标签页（一个都不知道时两个都显示），默认打开第一个；其他长度的窗口不画进图表。当前窗口没有对应历史系列时，前端补一条空样本行，使用当前剩余和重置时间显示标题，曲线区显示缺数据，不伪造样本。同一额度名下有自己标签的窗口（例如 Claude 的 `7 Day Opus`）单独成行，不与主窗口合并。
- **已停用窗口**：历史系列在当前状态中没有对应窗口时，且所选范围内仍有样本，单独显示为“已停用”。该行放在同一额度组的当前行后，不计入合计，不参与剩余最低排序；条带和曲线停在最后一个样本，不画匀速参考线和延长线。最后一个样本移出范围时隐藏；历史接口仍返回这些系列，由前端过滤。
- **排序**：图表默认跟随卡片顺序，同一额度的合计行使用其第一个当前账号的位置；同一账号内按 `group_order` 排列（Claude、GPT、Gemini、Fable、Claude / GPT，来自 `internal/providers/provider.go` 的 `groupOrder`）。也可按当前剩余从低到高排列。状态接口中账号优先按 `account_order` 中的 pin 排列，未列出的按服务注册顺序、账号后缀排列；不存在的条目忽略，重复条目以第一次为准。同一账号的额度组按额度组注册顺序排列，所以账号卡片上 Gemini 在 Claude / GPT 之前、Claude 在 Fable 之前。
- **记住筛选**：`remember_chart_filters` 开启时，页面把所选窗口、排序和每个窗口各自的时间范围存在 `localStorage` 的 `lamplighter-chart-filters` 中，打开页面时恢复；5 小时和 7 天的时间范围选项不同，所以分开保存。保存的值不在当前选项中时使用默认值。开关读取设置表单中的勾选状态，所以改动在保存前就生效；关闭时删除这项记录。
- **加载**：页面按所选窗口请求历史（`window` 参数），并按窗口和时间范围缓存响应。切换到缓存过的组合时立即画出缓存的数据，同时重新请求，响应到达后重画；没有缓存时图表变灰，等响应到达。只有最近一次加载的响应会画到图上，切换后才到达的旧响应只存入缓存；同一组合已有请求在进行时，加载沿用这个请求，刷新和点火后的加载除外。请求失败且没有所选组合的数据时，图表显示错误信息，不再显示其他组合的旧图。页面打开、第一张图加载成功后，预先请求另一个窗口的时间范围，切换窗口时不用等待。页面每 30 秒读取一次状态；某个窗口的 `observed_at` 晚于图表中该窗口的最后一个样本，并且来自主动查询或剩余值与最后一个样本不同时（数值不变的被动读数不写入历史），重新请求当前图表（`refreshHistory`）。鼠标在图表卡片内时只保存新数据，移开后再重画，以免替换鼠标下的竖线和提示。
- **重置**：相邻样本的剩余上升 5 个百分点及以上视为重置。曲线在重置处断开，前一段保持最后的值画到重置样本的时间，新的一段从重置后的值开始，图上标出重置时间；相隔一个样本的两次上升算一次重置。单个账号重置只让合计上升一部分，上升不足 5 个百分点时合计曲线不断开。
- **匀速参考线**：`rowPace` 使用历史接口的 `resets: [[t, x], ...]`，不从剩余额度的跳升反推周期。窗口长度 `L` 固定为 5 小时或 7 天，参考值为 `100 × (x − t) / L`。第 `i` 项只在 `[max(t_i, x_i − L), min(t_{i+1}, x_i)]` 内绘制，最后一项的结束时间为 `x_i`；`x_i=0` 时不画。提前重置时旧线在新记录处截断，保留截断处的参考值，新线按新的预定重置时间计算。状态接口中的当前 `reset` 与最后一项相差超过 60 秒，或历史没有已知重置时间时，从当前图表时间边界起补一项当前重置记录。当前窗口仍存在但 `reset` 未知时，传入 `0` 并补一项未知记录，旧参考线在该边界截断。从未知记录生效时刻起参考值为空，包含该时刻本身：`paceSegments` 用内部的第四项标记排除旧段右端，`paceAt` 对这类段采用右端开区间。绘图仍可延伸到截断边界，单账号和合计在该边界的标题匀速与差值均为空；当前窗口不存在时传入 `null`，不补写当前状态。此处的时间边界取历史响应的 `to`，与图表几何和采样范围一致。合计行只在所有当前账号都有参考值的区间按权重平均，任一账号缺少参考值时断开。
  - **往回延长**：一段参考线的起点晚于本周期起点时（周期中途预定重置变了，或此前重置时间未知），`paceExtensions` 把它往回延长到本周期起点：在该段重置时间减去窗口长度、再提前 60 秒（`RESET_DRIFT`）之后的样本中，最后一次回升 `RESET_JUMP`（5 个百分点）及以上的时刻；找不到这样的回升时不延长。前后两段的预定重置相差不超过 `RESET_DRIFT` 时视为同一条线，不延长。延长部分用与匀速参考线同色的点划线（`stroke-dasharray: 8 3 1.5 3`）。合计行在任一账号有延长时，用延长值代替该账号原来的参考值计算加权平均（`extendedPace`），仍要求每个账号都有值。只有画了延长线时，图例才显示说明，悬停提示多一行“匀速参考（往回延长）”。页头的匀速和差值、MCP 的计算不使用延长值。
  - **数值标注**：详情图标题行依次显示当前剩余、此刻参考值和差值（高于参考值为绿色，低于为红色）、距重置的时间；悬停提示显示该时刻的参考值。横轴右端是现在时，图表右侧标出曲线和参考线的末值及差值；两个数值离差值不足一行（`LABEL_H`）时向上下推开，差值保持在中间。
- **平滑**：同一窗口内额度只会下降，但主动查询和响应头的数值可能差 1 个百分点，取整方式也不同，样本会短暂回升。画线前把每段样本拟合成最接近的非递增序列（相邻违例合并取平均），否则每次回升都会在曲线上留下一个小缺口；悬停提示仍显示原始样本。曲线使用单调三次插值（Fritsch-Carlson），不会越过拟合后的值。额度按整数百分比上报，所以变化不超过 2 个百分点的台阶按连续下降连线；平台之后是更大的下降时，曲线先保持水平，再在下降处弯折。
- **概览条带的颜色**：条带按每个周期内的累计最低值着色，后面的样本不高于此前的最低值，遇到重置重新开始。Claude 响应头的数值总比主动查询高 1 个百分点（主动 50、被动 51），读数在阈值附近时，按原始样本着色会让条带短暂变色，留下细缝。这里不用平滑曲线的非递增拟合，它会把 50 和 51 平均成 50.5，变色的时间反而更长。详情曲线和悬停提示不受影响，悬停仍显示原始样本。
- **延长线**：横轴右端是重置时间、且最近的数据没有中断时，详情图从当前的点画一条点线（`projection`）。先降到 0% 时停在交点，在时间轴上标出用完的时间，7 天额度还标出日期，被它盖住的刻度标签不显示；先到重置时停在重置处，在图表右侧标出剩余额度；速度不是下降时画水平线。5 小时和 7 天额度用不同的速度，原因见下文“延长线的评估”。
  - 5 小时额度用当前速度（`currentRate`），表示照这个速度用下去的结果。速度取自本周期的样本（`floor` 最后一次上升之后）：先取累计最低值（与概览条带相同），再做自底向上的分段线性拟合，用最后一段的斜率（`recentSlope`）。拟合从每两个相邻样本一段开始，相邻两段合成一条最小二乘直线后，所有样本离直线都不超过 2 个百分点（`QUANTUM`）时合并，误差最小的先合并。额度按整数百分比上报，样本是台阶状的，相邻两个样本之间的斜率不是 0 就是很陡，所以不直接求导；回看多长由分段决定，用量变化处会切开。被动样本最多晚一分钟写入，最后一个被动样本超过 2 分钟（`HOLD_AFTER`）没有变化时，才把它的值延续到现在参与拟合，停用之后延长线逐渐变平。主动样本每个查询间隔写入一次，最后一个主动样本要超过 1.5 个采样间隔才延续到现在，否则每个查询间隔的后半段延长线都会变平。最后一个样本还没有延续到现在时，曲线停在这个样本，延长线和斜纹也从这里开始；如果曲线保持最后的值画到现在，延长线却仍按此前的速度下降，看起来就像向右错开了一段。延续到现在之后，曲线画到现在，延长线从现在开始。突然大量使用后的头几分钟，最后一段只有几个样本，估计的用完时间偏早，之后逐渐后移。
  - 7 天额度用本周期日均（`cycleRate`）：本周期已用的额度（100 减去 `floor` 的最后一个值）除以周期开始以来的时间。周期起点是状态接口中当前窗口的 `reset` 减去 7 天，不用样本中检测到的重置。没有重置时间、重置时间已过、周期还没开始，或最后一个样本早于周期起点时不画。合计行取各账号日均速度按权重的平均值，每个账号用自己的重置时间，只计入满足上述条件的账号。
  - 延长线的评估：用 2026-10-03 到 10-20 的全部历史逐周期回放，每个时刻只用当时已有的样本预测重置时的剩余额度，再与实际值比较。7 天额度（两个周期）用最后一段斜率的平均误差为 20.0 个百分点，用固定回看 24、48 小时为 13.5、12.3，用本周期日均为 9.9。7 天额度通常白天使用、夜间停用，最后一段只反映当晚的集中使用，用完时间估计得过早；固定回看时间同样受使用节奏影响，所以不用。5 小时额度（69 个周期）的各种外推都不比“之后不再使用”准确，之后还会用多少基本无法预测，所以保留当前速度。回放期间没有一次用到 0%，用完时间的准确度没有评估。
- **详情图标题行**：图表宽度不小于 640 px（左侧标签区 `labelW` 至少 190 px）、且标题行不高于图表时，`placeDetailHead` 把标题行竖排放进图表左侧与概览图对齐留出的空白，右侧留 `SIDE_GAP`（46 px）给纵轴刻度；否则标题行放在图表上方。
- **悬停**：在概览图或详情图上悬停时，竖线吸附到详情图当前行（多账号为合计线）离鼠标最近的采样点，提示中的时间和各行数值都取该时刻；采样时间不同的其他行显示该时刻之前最近的样本。最近的采样点也超过缺数据阈值时，竖线停在鼠标所在的时间。在现在右侧悬停时，竖线跟随鼠标，详情图的提示显示该时刻延长线的值和匀速参考值，概览图不显示提示。

每轮主动查询开始时，`engine.loadCreds` 读取账号列表，重新计算每个账号的后缀（`Cred.Suffix`）：名称（`account_names` 中的名称，否则是 Magpie 的 `label`，与邮箱相同时不算）的第一行（`accountName`，最长 `maxNameRunes` 个字符）不为空时为 `#` 加名称，与同一服务另一个账号的后缀相同时（例如名称重复）再加邮箱后缀；没有名称时，同一服务有多个账号才加邮箱后缀（`maskedSuffix`）。额度组的 `Label` 随之更新。告警状态按额度组 key 记录，改名不会重新发送通知。

## MCP

MCP 在对外监听的 `/mcp` 上提供，`mcp_enabled` 为真且 `mcp_key` 非空时开启；监听、重新绑定、关闭和状态字段见“HTTP 接口”的“对外监听”。连接及 HTTPS 代理示例见 [README](../README.md#mcp)。

`server.Server.apply` 先把配置交给引擎（`engine.Configure`），再调用 `mcp.Server.Configure`，最后调用 `engine.MCPConfigured`。所以顶层 `config_applied_token` 表示对外监听也已按这份配置执行；绑定失败并记录错误也算执行完成。监听是否可用读取 `status.public` 和 `status.mcp`；这些布尔值不能确认密钥已经更换，确认更换看 `config_applied_token`。配置无法解析时不调用 `Configure`，对外监听保持原来的设置。

### 协议与鉴权

服务采用无会话状态的 Streamable HTTP，支持 `initialize`、`notifications/initialized`、`ping`、`tools/list`、`tools/call`。请求版本为 `2025-03-26` 或 `2025-06-18` 时原样协商，不支持时建议 `2025-06-18`；后续 `MCP-Protocol-Version` 若存在必须受支持。POST 请求只返回 `application/json`，不建立 SSE；通知返回 202 且无正文，GET 返回 405。`2025-03-26` 接受最多 50 项的 JSON-RPC 批次，`2025-06-18` 拒绝批次。请求体最多 1 MiB，读取和写入有超时限制。每个服务实例最多同时执行 4 次工具调用，批处理内的每次调用也占用同一组名额；名额不足立即返回 JSON-RPC `-32000` 忙碌错误，调用方可稍后重试，不进入等待队列。`list_quotas`、`get_quota_history` 和 `get_quota_events` 的调用超时为 10 秒，`refresh_quotas` 为 180 秒。关闭监听、关闭 `/mcp` 或更换 `mcp_key` 会取消已登记的 MCP 请求，`mcp.Server.Close` 还会等待取消后的处理退出；历史扫描和预测计算沿用调用 context，在循环中检查取消。

每次请求验证 `Authorization: Bearer <mcp_key>`，先对双方求 SHA-256，再常量时间比较，避免泄露密钥长度和前缀。出现 `Origin` 时只接受 http/https 的 `localhost` 或回环 IP，不解析任意主机名，拒绝其他来源和多个 Origin。代理不能把外部 Origin 改写成本机来源。鉴权及 HTTP 传输错误使用对应状态码；JSON-RPC 解析、请求、参数和方法错误使用标准错误码，工具执行错误使用 `isError` 结果。

### 工具数据

`engine.MCPBackend` 使用管理状态的内存快照，但只输出专用 DTO。`baseAccountLabel` 优先取备注第一行，先替换当前账号列表中所有已知完整邮箱，再将剩余含 `@` 的连续非空白片段整体打码，用户名去掉引号后保留最后两个 Unicode 字符；没有备注名时取 `***` 加邮箱用户名的最后两个 Unicode 字符，用户名不足两个字符时保留全部，不包含域名或用户名长度；没有备注名和邮箱时使用 `account-序号`。`accountLabels` 为重名基础标识生成 `#n` 后缀，预留全部基础标识并避开已生成标识，确保完整标识唯一；`retired account` 为保留标识，同名现有账号也生成后缀。`selectedAccounts` 优先精确匹配完整标识，否则按基础标识、原始备注第一行或完整邮箱匹配；`retired account` 不选择任何现有账号，只用于退役历史筛选；共享名称可选中多个账号，刷新要求唯一匹配。列表、合计中的最早耗尽账号、历史及事件共用这些标识，不返回完整邮箱、凭证文件名、auth index、额度组内部 key、数据目录、配置和通知凭据。上游错误仅映射为查询失败、认证失败或限流，事件不转发原始错误、摘要、申诉链接及任意文本。事件按额度组 key 找到当前账号；凭证事件还带内部的账号 auth index（`Event.Account`，不输出到页面和 MCP），账号还没有额度组时（例如第一次查询就失败）按它找到当前账号的标识。时间对象包含 `iso`（RFC 3339 UTC）和 `relative`。

`CallTool` 在读取数据前检查受 `engine.mu` 保护的状态：`enabled == false` 时返回 `ready: false`、`state: "disabled"` 和 `certain: false`；引擎未运行、正在停止，或 `lastPoll` 为零且 `pollErrs[""]` 为空时，返回 `ready: false`、`state: "starting"`、`certain: false`、`retry_after_seconds: 30` 及说明。这些结果不附带数据，不设置工具错误，避免启动期间把未加载账号的历史误标为 `retired account`。凭证列表读取失败视为已就绪，以免启动状态无限持续。`refresh_quotas` 仅要求引擎运行；在账号匹配后，运行状态复查和限流占位共用 `engine.mu`，停止或停用先发生的请求不占用限流。未知工具仍返回错误。

- `list_quotas` 仅返回当前窗口，按服务及额度组汇总；每个账号带 `credential_invalid`（已确认的凭证失效，或最近一次查询为认证失败）和 `account_suspended`（已确认的账号停用），取自状态接口账号的 `credential_problem`；每个窗口的合计只计入拥有该窗口的账号，权重依次取 `account_weights`、套餐默认权重、1。给出每个窗口最先预计耗尽的账号。5 小时速度采用当前周期累计最低读数的分段最小二乘拟合，最大偏差 2 个百分点，最多 256 个输入点；7 天速度采用本周期累计使用量除以周期已过时间。缺失周期或样本时预测为空；超过 15 分钟或 3 个查询间隔（取较长者）未更新时标记过旧并停止预测。7 天匀速剩余为距重置占整个周期的比例，每日匀速预算为 `100/7`，同时给出实剩、差值和剩余每天可用量。滑动占位观测通过受锁保护的副本提供；不能确认窗口是否开始时返回空值。
- `get_quota_history` 与 `list_quotas` 使用可取消的流式历史查询，扫描时先按所选账号、服务和窗口筛选，再按时间桶只保留最后一个样本；`list_quotas` 只读取当前窗口的样本，避免退役系列占用预算。此过程不全量保留原始记录。桶长度至少为管理历史的分段长度和所选范围的 1/2000（按秒取整，最少 1 秒）中的较大者；聚合中间结果最多 20000 个桶，超限返回工具错误。结果排序和预测计算只处理有界集合。`get_quota_history` 保留消失的窗口，按服务、账号、窗口筛选后每个系列最多 200 点、单次最多 2000 点，采样保留首尾，返回格式不含管理历史的 `resets`。已删除账号的历史使用匿名的 `retired account` 标识。
- `refresh_quotas` 与 `POST /api/refresh` 一样交给引擎命令队列执行手动刷新（凭证已过期的账号先让 Magpie 刷新额度，见“额度记录”），全局每 60 秒一次（引擎运行期间已接受的刷新失败也计入，未运行或停用时不占用限流），超限给出等待秒数和时间。`/mcp` 上等待上限 180 秒，经 Magpie 的 tool 调用最长 50 秒；连接关闭或超时结束等待，已进入引擎的查询可能继续完成。
- `get_quota_events` 从最近保留的最多 200 条引擎事件中筛选重置、耗尽、点火成功/失败和凭证问题（`credential_invalid`、`credential_restored`），支持 `since` 和最多 100 条的 `limit`。事件带 `reason`（`auth`、`suspended`、`rate_limited`，只输出这三个取值），凭证事件还带 `source`（`ignite`、`query`）；账号还没有额度组时，凭证事件的 `group` 为空。重置和耗尽事件独立于通知渠道记录，窗口消失或首次返回不产生事件。

Magpie 在转发请求时按自己的规则选择账号。Lamplighter 读不到完整的模型到账号的路由，因此 `model` 查询保守地保留候选额度组并标记 `certain: false`；不根据模型名称猜测唯一账号。

## 服务

`internal/providers` 中每个服务是一个 `providers.Provider`，在 `provider.go` 的 `registry` 中按显示顺序注册：GPT（`codex`）、Claude、Antigravity。服务定义包含：

| 字段 | 内容 |
| --- | --- |
| `ID`、`Aliases`、`Title` | 配置键和额度组 key 使用的服务名；Magpie 中的其他写法；显示名 |
| `Monitor`、`Ignite` | 默认是否监控、点火 |
| `Fetch` | 主动查询，返回额度组 |
| `Headers` | 解析模型请求的响应头；为空表示响应头里没有额度，被动跳过对该服务不生效 |
| `Ignition` | 点火协议、请求体、候选模型、模型提示、可勾选的额度组；为空表示不点火，设置页显示“不支持点火” |

`providers.Lookup` 按服务名或别名查找（忽略大小写，`_` 视同 `-`）。凭证列表中的服务名换成 `ID` 存进 `Cred.Provider`，原始写法存进 `Cred.HostProvider`。配置中用别名写的服务键在解析时换成 `ID`。

**窗口种类**：`quota.Window.Period` 是窗口长度，由各服务从接口数据填入：Codex 的 `limit_window_seconds` 和 `Window-Minutes`，Claude 的字段名，Antigravity 的 `window`。`quota.Kind` 先看周期（与 5 小时或 7 天相差 5% 以内），没有周期时按 ID 和标签判断。5 小时和 7 天窗口使用固定的 ID 和标签（`five-hour`、`seven-day`），其他长度的窗口带自己的 ID，标签为 `quota.PeriodLabel` 给出的长度，例如 `30d`。点火、图表和通知都按种类处理，所以没有当前 5 小时窗口的账号（例如 ChatGPT Pro）不点火；图表仍可显示所选范围内保留的 5 小时历史窗口。

**新增服务**：在 `internal/providers` 中新建一个文件，实现 `Fetch`，有额度响应头时实现 `Headers`，窗口填上 `Period`；能点火时填写 `Ignition`；把服务加入 `registry`，需要固定位置时把主额度组的名称加入 `groupOrder`；在 `internal/web/page.css` 中加上该服务的标签颜色（`--type-<id>-bg` 等）。同步更新 README 的服务表和本文档的主动查询、被动数据、点火请求和模型选择各节。

## 后台循环

`engine.Engine` 用一个后台循环串行处理查询、点火和通知状态；HTTP 取消和模型流 watchdog 在独立 goroutine 中执行，不修改额度状态：

1. 程序启动后先等 5 秒（`startupDelay`），让同时启动的 Magpie 网关就绪。
2. 打开数据目录，取得 `instance.lock`。
3. 依次处理排队的被动数据、Did Codex Reset、到点的主动查询、Magpie 的额度记录（`pollKept`）、重置前后的查询、到期的点火、历史清理、新版本检查，并保存状态。
4. 计算下一次需要醒来的时间，在这之前等待新的被动数据、管理操作或配置变更。

Magpie 停用插件时会重启插件宿主，Lamplighter 随宿主的进程组一起退出，所以没有“停用”状态。

`ObservePassive` 和 HTTP 接口不直接执行网络请求：前者只把读数放入队列（队列满时丢弃，丢一个被动样本没有影响），后者把操作交给后台循环并等待结果，最多 3 分钟。唯一的例外是 `check-update`，见下文。

新版本检查读取 GitHub API 的 `releases/latest`（不含草稿和预发布版本），后台循环开始工作后读一次，之后每 6 小时读一次，失败后 30 分钟重试。页面打开时调用 `check-update` 再读一次，距上次尝试读取（无论成败）不到 1 分钟（`releaseRecheckPeriod`）时不读：未登录的 GitHub API 每个地址每小时只能请求 60 次，GitHub 不响应时也不会让每次打开页面都等到超时。这次读取在处理请求的 goroutine 中进行，不经过后台循环，以免排在查询和点火之后；插件停止时请求随之取消。同一时间只有一个读取（`releaseMu`）：已有读取在进行时，页面的请求直接返回，后台循环不等待并在 30 分钟后重试；距上次成功读取不到 1 分钟时，后台循环也不读。请求使用 Did Codex Reset 相同的出口（环境变量中的代理或直连）。版本号按数字逐段比较；最新版本比当前版本新时，状态接口返回 `latest_version` 和 `release_url`，页面在版本号右边显示更新图标。失败只记 debug 日志，保留上一次的结果。当前版本不是纯数字版本时（开发版本 `dev`、`0.0.0-dev.<commit>` 和预发布版本）不检查；最新 Release 的标签不是纯数字版本时视为没有新版本。

后台循环崩溃时，`supervise` 记录日志并在 30 秒后重启。未恢复的 panic 会结束进程，页面和 tool 随之不可用，所以每个 goroutine 都有 `recover`。

### 生命周期

- 启动时读取 `config.json`；`PATCH /api/config` 保存并应用新的配置。无法解析的配置不会写入文件。
- 收到 SIGINT 或 SIGTERM 时关闭监听、停止后台循环并退出。
- `instance.lock` 是数据目录中的独占文件锁（Unix 上用 `flock`，Windows 上以不共享的方式打开文件），保证同一时间只有一个实例在查询、点火和推送。新实例拿不到锁时每 10 秒重试一次。

## 额度获取

### 主动查询

| 服务 | 请求 |
| --- | --- |
| Codex | `GET https://chatgpt.com/backend-api/wham/usage`，带 `Chatgpt-Account-Id` |
| Claude | `GET https://api.anthropic.com/api/oauth/usage`，`anthropic-beta: oauth-2025-04-20` |
| Antigravity | `POST .../v1internal:retrieveUserQuotaSummary`，依次尝试三个地址，正文为 `{"project": "<project_id>"}` |

请求头常量在 `internal/providers` 中各服务的文件里。Codex 的 User-Agent 与 Codex CLI 一致，Claude 的 User-Agent 与 Claude Code 一致。

Antigravity 的 User-Agent 按以下规则确定。Cloud Code 按 User-Agent 判断客户端，对旧格式的客户端（如 `antigravity/cli/1.0.13 (aidev_client; ...)`）不返回 Gemini 额度组。

- 凭证设置了 `user_agent`（文件顶层、`attributes` 或 `metadata` 中）时使用它；以 `antigravity/` 开头的值去掉 ` google-api-nodejs-client/...` 后缀。
- 否则使用 `antigravity/hub/<版本> darwin/arm64`。版本读自 Antigravity Hub 更新清单（`AntigravityHubManifestURL`），成功后缓存 3 小时；读取失败时保留已缓存的版本，30 分钟后重试。还没有读到时使用 `2.23.0`。清单在查询额度时按需读取，不经过凭证代理。

token 来自 `host.Credential`。Claude 和 Codex 的 token 由 Magpie 或代理 CLI 刷新，Lamplighter 不刷新；上游返回 401 时跳过本轮，等它们刷新。401 持续下去时按凭证失效处理；401 的正文说账号已停用时（如 `account_deactivated`）第一次就按账号停用处理，见“凭证问题”。非 2xx 的响应（包括 401）返回 `quota.HTTPError`：错误文字（显示在页面上）只带响应的前 300 个字节，`Body` 保留前 4096 个字节，供 `failure.Diagnose` 读取靠后的 `reason`。状态码为 401 的 `HTTPError` 在 `errors.Is` 中等同 `quota.ErrUnauthorized`，Antigravity 遇到它不再换下一个地址。

查询时间对齐到绝对时间边界（`alignedAfter`），默认 300 秒间隔落在每小时的 `:00 / :05 / ... / :55`。不使用“上次时间加间隔”，以免时间点随启动时间漂移。

### 网络出口

`internal/egress` 让额度请求和 Magpie 对同一账号的请求使用同一个出口。`host.Credential.ProxyURL` 由 `internal/magpie` 按 Magpie 的规则（`ProxyOfLogin` 和 `netproxy`）确定：

1. `providers.json` 中该服务的 `accountProxies` 里该账号的代理（键为小写的账号名），否则该服务的 `proxy`。
2. 否则 `settings.json` 的全局代理 `proxy`。
3. 否则插件宿主启动时得到的代理。Magpie 把它放在 `MAGPIE_HTTPS_PROXY`、`MAGPIE_ALL_PROXY`、`MAGPIE_HTTP_PROXY` 中（SOCKS5 代理已桥接成 HTTP），宿主的环境中没有 `HTTPS_PROXY`。
4. 都没有时为空。

`direct` 表示直连。`@<id>` 是 Settings 中保存的代理，换成 `settings.json` 的 `proxies` 中该代理的地址和登录信息；这个代理已不存在或是 SSH 服务器时返回错误，不改用其他出口。只写 `host:port` 时按 HTTP 代理处理。`egress` 按以下规则使用 `ProxyURL`：

- 支持 `http`、`https`、`socks5`、`socks5h` 代理；`direct` 或 `none` 表示直连。无法解析的地址返回错误（错误中隐藏用户名和密码），不退回其他出口。
- 为空时使用环境变量代理（`HTTPS_PROXY` 等），没有时直连。
- 每个请求的超时为 `request_timeout_seconds`。

### 套餐

状态接口的账号带 `plan`（套餐名，未识别时为空）和 `default_weight`（多账号合计中的默认权重），由 `providers.PlanOf` 按保存的原始套餐值得出。原始值按 `auth_index` 存在 `state.json` 的 `plans` 中，插件重启后不必等到下一次查询。默认权重是账号额度相对于该服务最便宜付费套餐的倍数。

- Codex：取 `wham/usage` 返回的 `plan_type`，每次主动查询都更新，不另发请求。套餐名与 Codex CLI 的状态页一致（`codex-rs/tui/src/subscription.rs`），默认权重为价格除以 Plus 的 20 美元：`prolite`（Pro $100）和 `self_serve_business_prolite`（Business Premium）为 5，`pro`（Pro $200）为 10，`promax`（Pro $500）为 25，`free` 和 `go` 为 0.1，其余为 1；Business（`team`）、Enterprise 和 Edu 席位的额度与 Plus 相同。未知的 `plan_type` 以原值作为套餐名，权重为 1。
- Claude：额度响应里没有套餐。主动查询成功后请求 `GET https://api.anthropic.com/api/oauth/profile`，请求头与额度查询相同。请求失败时保留上次的结果，只记 debug 日志，不影响额度查询。原始值是 `organization.organization_type` 和 `organization.rate_limit_tier` 以空格连接：`claude_pro` 为 Pro（1）；`claude_max` 的 tier 含 `20x` 为 Max 20x（20），含 `5x` 为 Max 5x（5），都不含时为 Max（5）；`claude_team`、`claude_enterprise` 为 Team、Enterprise（1）。没有 `organization_type` 时按 `account.has_claude_max`、`has_claude_pro` 判断。2026-10-20 在 Pro 账号上实测为 `claude_pro` 和 `default_claude_ai`；Max 和 Team 的取值来自 Claude Code 的已知用法，未经实测。
- Antigravity：额度响应里没有套餐。主动查询成功后请求 `POST https://daily-cloudcode-pa.googleapis.com/v1internal:loadCodeAssist`，请求体为 `{"metadata":{"ideType":"ANTIGRAVITY"}}`，User-Agent 与额度查询相同。原始值是 `paidTier.id`，没有 `paidTier` 时为 `currentTier.id`。Google AI Pro 账号的 `currentTier.id` 也是 `free-tier`，所以以 `paidTier` 为准。`free-tier` 为 Free，`g1-<名称>-tier` 取中间的名称并大写首字母，例如 `g1-pro-tier` 为 Pro；其余以原值作为套餐名。Google 没有公布各套餐额度的倍数，权重都是 1。2026-10-23 在 Google AI Pro 账号上实测 `paidTier.id` 为 `g1-pro-tier`；没有付费套餐时不带 `paidTier` 是推测，未经实测。
- 其他服务没有套餐，默认权重为 1。

Claude 和 Antigravity 的单独请求（`Provider.Profile`）每个账号每 24 小时发一次（`plans` 中的 `checked`）。账号的主动查询结果与上一次相比有额度窗口增减时（比较 `plans` 中的 `windows`），之后 1 小时（`planRecheckPeriod`，记在 `recheck_until`）内每次主动查询都读一次套餐，直到读到套餐变化：Google AI Pro 到期后，Antigravity 的接口不再返回 5 小时窗口，7 天窗口换成免费账号的新额度。读取可能失败，套餐也可能比窗口晚一步变化，所以不只读一次。`windows` 保存在 `state.json` 中，插件停止期间发生的变化在重启后的第一次查询就能发现。重读期间读取失败时，这一轮不处理这个账号的额度提醒（只记录样本），等下一轮读到套餐再比较，因为这时的窗口可能属于一个还没读到的新套餐。从未读到过套餐的账号不暂缓：之后读到套餐也只算第一次读到，不会重新记录基线。重读期间读取失败后，15 分钟（`planRetryPeriod`）后的第一次主动查询会再读一次，1 小时结束后也是如此，不必等到 24 小时后。读取连续失败超过 1 小时后，额度提醒恢复按旧基线比较。套餐比窗口晚一步变化时（窗口变化的那一轮读到的仍是旧套餐），这一轮的额度提醒按旧基线比较，可能发出一条错误的恢复或下降提醒，读到新套餐的那一轮才推送套餐变化。

**套餐变化**：`engine.notePlan` 在处理额度提醒之前读取套餐。保存的原始值变了，并且新旧套餐名不同时，`engine.planChanged` 记录 `plan_change` 事件，把待发送的通知记在 `plans` 的 `notice` 中，并把这个账号所有窗口的通知基线标为 `rebase`。`notify.Alerts.ProcessGroup` 遇到 `rebase` 的窗口时与第一次看到的窗口一样只记录基线，不推送，但保留已发送的重置提醒（`reset_notice_1h_for`、`reset_notice_1d_for`）和 `ended_reset`，所以同一次重置不会再提醒一次。新套餐的额度与旧套餐无关：不重新记录基线的话，到期后免费账号的新 7 天额度会被当成恢复，续订后恢复的原有额度会被当成下降。`plan` 类通知（`timeSensitive`）随每次主动查询发送，失败时重试 1 小时（`planNoticeAge`）；通知送达前套餐又变回原来的，两次变化都不推送。第一次读到套餐（原来没有保存的值）时只保存，不推送。Claude 的被动数据不经过 `notePlan`：每天读取套餐之前，套餐变化后的被动数据仍按旧基线比较。

### 被动数据

被动数据有两个来源：Magpie 的额度记录（`KeptQuotas`，见“与 Magpie 的接口”），以及点火响应的响应头。响应头按下表解析：

| 服务 | 响应头 | 窗口 |
| --- | --- | --- |
| Claude | `Anthropic-Ratelimit-Unified-5h-Utilization` / `-5h-Reset`，`-7d-` 同理 | 5 小时、7 天；值是已用比例，如 `0.2` |
| Claude | `Anthropic-Ratelimit-Unified-7d_oi-Utilization` / `-7d_oi-Reset` | Fable 的 7 天窗口，写入与主动查询相同的 Fable 额度组（`providers.FableGroupKey`）。普通（非 Fable）请求的响应头里没有这组；Fable 请求时是否返回，还没有在能用 Fable 的账号上验证过。插件每次启动后第一次读到时记一条日志 |
| Codex | `X-Codex-Primary-Used-Percent`、`-Reset-At`、`-Reset-After-Seconds`、`-Window-Minutes`，`Secondary` 同理 | 按窗口长度区分，300 分钟为 5 小时，10080 分钟为 7 天，其他长度各自成为一个窗口；没有长度时 Primary 为 5 小时、Secondary 为 7 天 |

Antigravity 的响应头里没有额度。

被动数据只更新响应头里有的窗口，主动查询结果替换整个凭证的额度组及窗口；未返回的额度组保留空组，避免被动响应头重新补回。主动结果缺席的持久化窗口基线标记 `Rebase`，跨重启保留；窗口重新出现后的第一次读数只作为新起点，不触发恢复或重置前提醒。窗口消失本身不发送通知；没有 5 小时窗口时 `collectTargets` 跳过点火，恢复后自动继续。管理历史仍包含过去出现的所有窗口，由页面决定显示哪些窗口。额度组有过主动查询结果后，被动数据只更新那次结果中出现过的窗口，不新增窗口：ChatGPT Pro 账号的额度接口只返回 7 天窗口，页面上却出现过一个没有重置时间、一直是 100% 的 5 小时窗口，它只能来自响应头。最近一次成功主动查询的窗口集合保存在 `state.json` 的 `groups.active_windows` 中：`null` 或字段缺失表示尚未确认，空数组表示已确认整组缺席。被动数据只接受保存集合中的窗口，启动后首次主动查询失败也沿用该集合；只有新的成功主动查询能恢复缺席窗口。旧状态缺少 `active_windows` 时，优先从非空且格式有效的 `plans.windows` 恢复集合；这个签名只在成功主动查询后写入，组未出现在签名中表示该组缺席。空签名无法区分尚未查询和主动返回空集合，不作为迁移依据。无法可靠迁移的已有组在首次主动查询成功前，禁止被动数据新增窗口；当前内存视图中已有的窗口仍可更新，首次成功主动结果只建立新的通知基线。迁移能确认缺席的旧窗口同样标记为首次返回时重建基线，即使旧文件尚未设置 `Rebase`。此限制在加载时标记，不通过通知基线、`Rebase` 或点火观测推断窗口存在与否。状态文件中没有的真正新组允许被动数据先建立窗口。服务新开的窗口要到下一次主动查询才出现。点火响应的 `-Reset-After-Seconds` 从收到响应的时刻起算。

每个窗口记下当前读数的读取时间（`WindowView.ReadAt`）：主动查询为查询开始的时刻，Magpie 的额度记录为它的读取时间（`KeptQuota.ReadAt`），点火确认为点火请求开始的时刻。被动数据的读取时间早于窗口当前读数时，`applyGroups` 丢弃这个窗口的读数；一组窗口全部被丢弃时，不写历史样本，也不处理通知。多个请求并发时，先开始、后结束的长请求会在较新的读数之后带回较高的额度；额度下降较快时，两者相差可能达到 `RecoveryJump`，会被当成恢复，同一档提醒会再推送一次。主动查询结果不受这条规则限制。

跳过规则（`shouldSkipActive`）：响应头里没有额度的服务（Antigravity）从不跳过，Magpie 也不为它提供额度记录，见“与 Magpie 的接口”的“额度记录”。其他服务在查询时刻前 `passive_skip_seconds` 秒内有被动数据，且距上次主动查询不足 `passive_skip_max_minutes` 分钟时跳过。只看查询时刻之前的一小段时间，最坏情况下两次数据间隔为查询间隔加 `passive_skip_seconds`。手动刷新不跳过。

## 点火

### 时间

`ignite.Schedule.DueAt` 按以下规则给出下一次点火时间：

1. 当天 `start_hour` 之前：等到 `start_hour`。
2. 超过 `end_hour + end_grace_minutes`：等到第二天 `start_hour`。
3. 5 小时窗口是滑动占位：立即（受冷却和重试时间限制）。
4. 重置时间在未来：`reset + grace_seconds`；不在当天时段内时等到第二天。
5. 重置时间已过或未知：立即（受冷却和重试时间限制）。

同一额度组中有 5 小时以外的窗口用完（剩余 ≤ 0.01%）且尚未到重置时间时，定时点火跳过这个额度组，不尝试、不计失败、不推送；引擎在该窗口的重置时间醒来，由下一次查询确认额度是否恢复。页面在点火状态中显示被哪个窗口挡住（`blocked_window`、`blocked_until`）。手动点火不受此限制。

**滑动占位**：有些服务在窗口尚未开始时返回“当前时间加 5 小时”的重置时间，并且每次查询都向后移动。`ignite.Observe` 用连续两次主动观测判断：重置时间距当前约 5 小时（误差 120 秒），且两次观测间的移动量接近经过的时间（误差为经过时间的 2%，在 3 到 10 秒之间）。被动数据来自真实请求，请求本身会开始窗口，所以被动观测永远不是滑动占位。看到固定的未来重置时间会清空失败状态。

### 请求

点火通过 `host.Host.OpenModelStream` 经 Magpie 网关发送，`Account` 为账号的 pin，`Protocol` 为下表的入口协议，`Model` 为网关的模型 ID（`<服务>/<模型>`，见“模型选择”）。请求体由各服务的 `Ignition.Body` 给出，经 `providers.Request` 编码。各服务的请求如下：

| 服务 | 入口协议 | 网关路径 | 请求要点 |
| --- | --- | --- | --- |
| Codex | `openai-response` | `POST /v1/responses` | `reasoning.effort: none`，`tools: []`，`stream: true` |
| Claude | `claude` | `POST /v1/messages` | `stream: true` |
| Antigravity | `openai` | `POST /v1/chat/completions` | `temperature: 0`，`reasoning_effort: none`，`stream: true` |

所有点火请求都省略输出 token 上限字段。唯一的提示词由 `providers.TriggerPrompt` 给出：

```text
Reply with only OK. Do not think, do not use tools, do not do anything else.
```

`internal/magpie` 的点火请求（`gateway.go`）经“网关请求”一节所述的客户端发送：

- **锁定账号**：请求头 `X-Magpie-Account: <pin>`。pin 含非 ASCII 字符时按百分号编码发送，Magpie 会解码。锁定后网关只尝试这个账号，不换账号重试；账号不能接受时由网关直接回答，见“失败分类和保护”。
- **错误状态码**：网关返回非 2xx 时，`OpenModelStream` 返回该状态码和响应头，以及 `gatewayError` 给出的 `host.Error`。
- **模型列表**：`Models` 读 `GET /v1/models` 的 `data[].id`，最多读 16 MiB，20 秒超时。没有服务前缀的 ID（用户在 Magpie 中开启了只列出模型名）按 `owned_by` 补上前缀；`data` 缺失或某项没有 `id` 时返回包装 `host.ErrFormat` 的错误。

Claude 账号的请求在 Magpie 中由本机的 Claude Code 转发（Magpie 文档 `docs/subsystems/claude-subscription-bridge.md`，源码 `serveClaudeSubscription`）。`X-Magpie-Account` 在选择候选账号时生效（`pinTo` 在转发之前执行），Claude Code 随后在该账号保存的配置目录中运行，所以锁定对这条路径同样有效。Claude Code 向 Anthropic 发送真实的模型请求，按 Anthropic 的规则这会开始 5 小时窗口；请求带有 Claude Code 自己的系统提示词，输入 token 比直接请求多。网关把 Claude Code 报告的额度写回响应头 `anthropic-ratelimit-unified-{5h,7d,7d_oi}-{utilization,reset}`（`limitHeaders`），在请求到达时和回复开始时各写一次；网关在等待 Claude Code 启动期间可能提前发出响应头，这时响应头里还没有新窗口，由“确认”中的主动查询确认。以上来自 Magpie 源码和文档，尚未用真实账号验证。

### 流式执行与取消

`engine.executeStream` 同步打开模型流，取得流 ID 后逐块读取；响应头保留给 `confirm`。网关对三种协议都返回标准 SSE 字节流，`ReadModelStream` 每次返回收到的字节（最多 32 KiB），可能在任意位置切开：

- `openai`（Antigravity）：Chat 的每个 chunk 是一条 `data:` 行，以 `data: [DONE]` 结束。没有 `[DONE]` 时，收到 `finish_reason` 之后的 EOF 视为正常结束，以便读到单独发送的 usage chunk。
- `claude`：Anthropic 的事件（`event:` 和 `data:` 行），以 `message_stop` 结束。
- `openai-response`（Codex）：Responses 的事件，以 `response.completed` 结束。

`stream_parse.go` 对三种协议使用同一个 SSE 解析器：保留跨块的行内容，以空行派发事件，忽略 `event:` 和注释行（网关等待上游时会发送 `: keepalive` 注释，Responses 还会重复发送 `response.in_progress`），不提前派发没有换行的 `data:` 行。允许跨块的行、事件、CRLF 和 UTF-8 字符，单行及单事件缓冲各最多 1 MiB。首次非空文本、可见 reasoning 或工具参数增量触发 `onOutput`，心跳、角色、生命周期和加密 reasoning 不触发。

独立 watchdog 从首次输出的单调时钟时间起计时 2 秒（`ignitionOutputWindow`），到期调用 `CloseModelStream`，取消到网关的 HTTP 请求，阻塞中的 `ReadModelStream` 随之返回错误。读路径和 watchdog 通过互斥锁争取单一终止所有权，只有仍活动的流能被 watchdog 截断；已确定的完成或错误不会被迟到的截止、取消或 watchdog panic 覆盖。截止分支发现取消或停止已就绪时按取消处理。`sync.Once` 保证只关闭一次，调用返回前等待 watchdog 退出；关闭错误或 panic 记为 `close_error`，同时保留已确定的执行错误。主动 2 秒断流记为 `output_timeout`，不触发候选模型 fallback，随后仍走额度窗口确认。Claude 和 Responses 在终止事件之前的 EOF 作为异常处理（`unexpected_eof`）。

每次点火从发出请求到读完正文最长 2 分钟（`ignitionStreamTimeout`）：网关等待较慢的启动（如 Claude Code）时持续发送 keepalive，流可能长时间没有输出，不加限制会挡住后台循环。超时、调用 context 取消和插件停止都会取消请求，也包括还在等待响应头的请求；超时记为 `timeout`，返回 `errIgnitionTimeout`，按临时错误重试。

流内错误统一转为 `host.Error`，交给 `ignite.Classify`、`ignite.ModelRejected` 和既有失败保护处理。网关在发出 200 和响应头之后才遇到错误时，只能把它写成流中的错误事件（Magpie `streamError`）：Chat 为 `{"error":{"message","type","code"}}`，Claude 为 `error` 事件，Responses 为 `response.failed`（代码默认为 `server_error`，过载为 `server_is_overloaded`）。解析时只保留白名单内的 HTTP 状态码和协议 `code`/`type`，不从自由文本 message 中猜测分类，唯一的例外是网关附加的固定后缀：状态码为 408、429 或 5xx 时，网关在 message 末尾加上 `(429: you can retry your request)`（`saysPassing`），解析器只读取锚定在末尾的这个后缀。已知协议标识可补足状态码，例如 `rate_limit_exceeded` / `rate_limit_error` / `usage_limit_reached` 对应 429、`authentication_error` 对应 401、`model_not_found` 对应 404、`server_is_overloaded` 对应 529；显式状态码优先。宿主字符串错误（`StreamChunk.Error`）只解析 JSON 错误对象或开头的 `HTTP NNN` / `status NNN`，`HTTP NNN: {...}` 时同时解析后面的 JSON。账号停用另按固定白名单保留：`code`、`type`、`status`、`reason` 和 Google `details[].reason` 中的停用代码（`failure.SuspendedCode`，如 `TOS_VIOLATION`、`account_deactivated`）写进 `host.Error.Code`；message 含 `failure.SuspendedPhrase` 中的说法时，只在 `Message` 后附上这条固定说法；`details[].metadata.appeal_url` 只保留 `https://forms.gle/…` 和 `https://support.google.com/…` 形式的地址，放在 `host.Error.AppealURL`。没有显式状态码的停用按 403 处理，不按 `invalid_request_error` 补成 400，以免被当成模型被拒。未知标识、原始 message 和原始错误载荷均丢弃。流内 401/403/429 不换模型，按下文“失败分类和保护”处理；400/404 沿用每次点火至多一次的模型 fallback。

### 调用日志

每次模型调用结束后写一条 `Lamplighter ignition` JSON 日志，启动失败也记录。JSON 放在 `host.Log` 的 Message 中，同时附带 structured fields：日志输出可能丢弃 fields，不能只依赖 fields。`json.Marshal` 转义换行及控制字符，正文不会伪造额外日志行。日志不写入 UI 事件、历史或状态文件。

记录包含 `provider`、`model`、`auth_index`（账号 pin）、随机 `correlation_id`、`stream_id`、`status_code`、`reply`、`reply_truncated`、`output_received`、`usage`、`usage_source`、`usage_completeness`、`end_reason`、`first_output_wait_ms` 和 `total_ms`。未收到输出时等待时间为 `null`，总耗时仍包含启动等待。`reply` 按收到顺序拼接文本、可见推理和工具参数，最多 4096 个 Unicode 字符，不截断 UTF-8 字符。只保留这些可见字段和白名单内的 usage 计数，不记录完整请求、凭证、原始错误载荷或 opaque 加密推理。

`usage_source` 固定为 `magpie_gateway`。`usage` 保留网关返回的白名单计数和已知细分项，插件不推算缺失值、不估算 token；这些数字可能已经过 Magpie 的协议转换，不能据此断言全部来自真实且完整的上游统计。Claude 的 `message_start` 和 `message_delta` 计数合并。

`usage_completeness` 为 `missing`（没有计数）、`partial` 或 `complete`。只有 Claude/`claude` 和 Codex/`openai-response` 原生协议的点火正常终止，且收到最终输入和输出计数时标为 `complete`；它表示网关协议的最终计数齐全，不保证每个上游细分项均存在。其余有计数的调用均为 `partial`，包括主动断流及 Antigravity/Chat 的自然结束：网关在协议转换中可能为缺失的计数补零，不同转换路径对推理 token 的合并和细分也可能不同，字段齐全或收到 `finish_reason` 不足以验证上游统计完整性，插件不把补出的零解释为确定的零用量。

`end_reason` 为 `completed`（正常完成）、`output_timeout`（主动断流）、`canceled`、`timeout`（超过 `ignitionStreamTimeout`）、`start_error`（请求没有发出或没有响应）、`http_error`（网关返回错误状态码）、`read_error`、`stream_error`（流内错误或无法解析）、`unexpected_eof`、`close_error` 或 `watchdog_error`。

### 模型选择

模型列表来自网关的 `GET /v1/models`（`host.Host.Models`，`models.Lister` 缓存 10 分钟），是所有服务所有账号模型的合集。Magpie 以 `<服务>/<模型>` 命名模型，服务部分是 Magpie 的服务 ID，例如 `codex/gpt-5.5`、`claude/claude-haiku-4-5`、`agy-cli/gemini-3-flash`。锁定账号的请求只能使用该账号所属服务的模型，因此点火时只取账号的 Magpie 服务（`Cred.HostProvider`）前缀下的模型（`credModels`），去掉前缀后按名称挑选候选（各服务的 `Ignition.Candidates`，由 `providers.Candidates` 去重并处理指定模型），再按完整 ID 发送。没有服务前缀的 ID 原样使用。

- Codex：名称含 `luna` 的模型由新到旧，然后其他 `gpt-` 模型；排除图片模型和 `gpt-oss`。
- Claude：`claude-` 开头的非 thinking Haiku 由新到旧，然后 Sonnet。
- Antigravity Gemini 组：Flash 由新到旧，然后其他非 Pro 模型，Pro 最后；排除图片模型。
- Antigravity Claude / GPT 组：Haiku、Sonnet、Opus（均为非 thinking）、GPT-OSS、其他非 thinking 模型。

“由新到旧”比较模型名中的数字段，新版本无需改代码。每次点火都从最新的候选开始。新模型发布后，旧模型通常还会在列表里保留很久，所以候选顺序不参考上次成功的模型。配置了 `model` 或 `models` 时只用指定模型，且它必须在该服务的列表中；指定模型可以写模型名，也可以写带服务前缀的完整 ID。模型列表暂时读不到时，使用指定模型（模型名补上账号的服务前缀）或上次成功的模型。`last_model`、事件和日志中的模型都是完整 ID。

候选模型不在锁定账号的套餐中时，Magpie 在本地回答 400 `X-Magpie-Account: <账号>'s plan doesn't list <模型>`；没有任何账号提供它时回答 404 `X-Magpie-Account: no account "<pin>" serves this model`。两者都不请求上游，插件换下一个候选，最多 6 个。全部被拒绝时按高风险错误处理（`ErrNoModel`）。

Magpie 为同一服务的所有账号列出相同的模型，所以列表里的新模型不一定能被每个账号调用。上游以 HTTP 400 或 404 拒绝某个候选时（`ignite.ModelRejected`；`failure.Diagnose` 判为账号停用的错误除外，停用的账号拒绝所有模型），插件在这次点火中换下一个候选，每次点火只换一次；再次被拒说明问题在请求或账号，按高风险错误处理，错误文字带上第一个被拒的模型和状态码；其余候选都被 Magpie 本地拒绝时，带上被拒的完整错误。401、403 和 429 与模型无关，不换模型。

换到的候选成功时，被拒的模型确实与这个额度组的账号有关：`engine.noteRefused` 在内存中记下额度组和模型，并记录 `model_refused` 事件；之后 24 小时（`refusedFor`）内，这个额度组的自动选择跳过该模型（`engine.skipRefused`），到期后再试一次。反复请求账号没有权限的模型可能增加账号风险，而新模型可能分批开放，所以按天重试。记录不写入 `state.json`，插件重启后清空。指定模型不受影响。

模型列表在两种情况下读取：程序启动后（`engine.maybeReadModels`），以及每次点火时。额度查询不读取模型列表；启动后的读取失败时，随之后每轮额度查询重试，直到成功。每次读取后，`engine.noteModels` 不考虑指定模型，按服务算出自动选择的第一个候选（取该服务及其别名对应的 Magpie 服务前缀下的模型），以完整 ID 放在状态接口的 `next_models` 中（每个能点火的服务一项），设置页在点火模型输入框的提示文字后显示它。额度组按服务的 `Ignition.HintGroup` 计算：Antigravity 按 Gemini 组计算，与提示文字“最新的 Flash”一致。`next_models` 只保存在内存中。读取失败时保留上次的结果。`next_models` 按服务计算，不考虑某个额度组跳过的模型。

### 确认

点火请求返回 2xx，且流正常结束或被 2 秒计时主动关闭后，插件确认 5 小时窗口在请求之后观测到了固定的未来重置时间。收到正文或 usage 本身不能确认成功：

1. 响应头里带额度时立即确认：Claude 的 `anthropic-ratelimit-unified-5h-*` 由网关写回（见“请求”）；Codex 的 `x-codex-*` 是否经网关转发到客户端尚未验证。
2. 否则按 5、10、15、30、15 秒的间隔检查，每次检查前主动查询该凭证一次；等待期间继续处理排队的被动数据。Antigravity 的响应头里没有额度，只靠这一步确认。

全部检查都没有确认时，返回 `ErrNotConfirmed`。

### Magpie 的预热

Magpie 自带的预热（内置插件“Warm-up and check-in”的设置，由 Magpie 的 `provider.KeepCodexWindowsWarm`、`KeepClaudeWindowsWarm` 执行）也会在窗口重置后或在设定的时间点发送请求开始窗口，与 Lamplighter 的点火同时开启会重复点火。Magpie 的预热只支持 Codex 和 Claude。

`magpie.Host.MagpieWarmup`（`internal/magpie/warmup.go`）读取 Magpie 配置目录的 `settings.json`：

| 字段 | 含义 |
| --- | --- |
| `codexWarmup`、`claudeWarmup` | `""` 或 `off` 关闭，`week` 只在 7 天窗口重置时预热，`all` 同时预热 5 小时窗口 |
| `codexWarmAts`、`claudeWarmAts` | 每天开启 5 小时窗口的时间点列表（如 `06:00`）；列表不存在时读旧版 Magpie 写的单个时间 `codexWarmAt`、`claudeWarmAt`，空列表表示关闭 |
| `codexWarmAtOf` | ChatGPT 账号自己的时间点，以小写的 Magpie 账号名（`host.Account.User`，工作区席位为 `a@b.com · team`）为键，`off` 表示不预热，代替 `codexWarmAts` |

预热设为 `all` 或设置了时间点的账号会被 Magpie 开启 5 小时窗口；只设 `week` 不调度 5 小时窗口，不算冲突。文件不存在时视为没有预热；格式不符（不是 JSON 对象、字段类型不对、预热模式不是上述取值）时返回包装 `host.ErrFormat` 的错误。账号按 Magpie 服务 ID 匹配，`codexWarmAtOf` 按账号的 `User` 匹配（没有时用 `Email`），所以同一邮箱的个人账号和 Team 席位各自对应自己的设置。

状态接口的顶层字段 `magpie_warmup` 列出 Lamplighter 会点火、Magpie 也开启了这类预热的账号 pin，按字符串排序，没有时为空数组（不为 `null`）。Lamplighter 会点火的账号是：`ignition.enabled` 开启，且账号至少有一个额度组是点火目标（`groupIgnitable`：服务的 `ignite` 开启，`groups` 为空或包含该额度组的名称）。`ignition.enabled` 关闭时不读取设置，直接为空数组；还没有查询到额度组的账号不列出。引擎通过可选接口 `warmupReader` 调用它，不属于 `host.Host`：测试或其他宿主不实现时为空数组。每次读取状态都重新读取设置，用户在 Magpie 中改动后下一次刷新即生效；读取失败时为空数组，只记 debug 日志。页面据此提示关闭 Magpie 的预热。

### 失败分类和保护

手动点火失败只记录一次 `ignite_failed` 事件并向调用方返回错误，不安排重试、不累计失败次数、不打开熔断或发送暂停通知；已有自动点火的重试、熔断、失败和凭证问题状态保持不变。手动点火确认阶段的额度查询（`queryCred` 不带 `track`）同样不改凭证状态：查询失败不建立凭证问题、不暂停自动点火、不通知，查询成功也不结束凭证问题，留给之后的轮询。事件的 `params` 为 `manual: "1"`、`error`、`summary`，没有 `retry_seconds`，页面据此不显示重试提示。手动成功仍通过 `ignite.RecordSuccess` 记录并清空失败状态，并结束账号的凭证问题。

Magpie 对带 `X-Magpie-Account` 的请求，在账号不能接受时自己回答，不请求厂商（Magpie 源码 `internal/gateway/fallback.go` 的 `pinTo`）。这些回答的 message 都以 `X-Magpie-Account: ` 开头：

| 状态码 | message | 含义 |
| --- | --- | --- |
| 429 | `<账号> rests until <RFC 3339 时间> (<原因>); no other account is tried in its place` | 账号在 Magpie 中休息：Magpie 在限流、额度用完或被拒之后让账号休息到厂商的重置时间 |
| 429 | 含 `usage cap reached` 或 `spend cap reached` | 用户在 Magpie 中给账号设置的上限 |
| 400 | `<账号>'s plan doesn't list <模型>` | 账号的套餐不含该模型 |
| 404 | `no account "<pin>" serves this model; …` | 没有这个账号，或它不提供该模型 |

前两种由 `ignite.PinHeld` 识别，后两种使模型不可用。

以下调度和保护规则用于自动点火。`ignite.Classify`：

| 类别 | 条件 | 处理 |
| --- | --- | --- |
| 换模型 | Magpie 本地回答的 400 `plan doesn't list`、404 `serves this model` | 换下一个候选 |
| 上游拒绝模型 | 高风险错误中的 HTTP 400/404（`ignite.ModelRejected`） | 每次点火换一次候选，之后按高风险处理 |
| 凭证失效、账号停用 | `failure.Diagnose` 给出 `auth` 或 `suspended`，见下文“凭证问题” | 暂停到凭证变化或手动点火成功 |
| 高风险 | `ErrNotConfirmed`、`ErrNoModel`，HTTP 400/401/403/404/409/422/429，`not found`、`unsupported` 等 | 立即暂停到第二天 `start_hour`；429 发送限流通知 |
| 临时 | Magpie 本地回答的 429（账号休息或达到上限）、点火超时，其他错误 | 按 `failure_retry_seconds × multiplier^(n-1)` 重试，第 `max_transient_failures` 次暂停到第二天 |

账号在 Magpie 中休息、且距休息结束不超过 10 分钟时（`ignite.CooldownWait`，取 message 中最晚的 `rests until` 时间），不计入失败次数，在休息结束后再等 `grace_seconds` 重试；事件带 `cooldown: "1"`。Magpie 在 429 之后让账号休息到厂商的重置时间，额度在重置前用完时，重置后 3 秒的点火会撞上这段休息。连续第二次休息按普通临时错误计数，避免休息不断续期时无限重试。

暂停时推送一次通知（`circuit_notified_until_epoch` 防止重复），成功或看到固定的未来重置时间后清空失败状态。通知和事件详情只写 `failure.Diagnose` 提炼的一句话（`summary`）：上游的完整错误是多行 JSON，只保留在事件的 `error` 和状态的 `last_error` 中。HTTP 429（不含 Magpie 本地回答的 429）暂停时发送限流通知代替暂停通知，并记录 `reason` 为 `rate_limited` 的 `credential_invalid` 事件。

### 凭证问题

`failure.Diagnose` 从错误中读出 HTTP 状态码、错误代码（Google `details` 中的 `reason` 优先，其次是 `code`、`type`、`status`）、申诉链接（`metadata.appeal_url`）和一句话摘要（上游 message 的第一句，没有 JSON 时为错误文字的第一行，最多 160 个字符），并分成三类。响应在 4096 字节处截断、JSON 解析不了时，仍从中取完整的 `message` 字符串。取不到可读的说明时（JSON 没有 message、结构未知、截断在 message 中间，或只剩状态码），摘要为固定的“HTTP 状态码 · 错误代码”（缺少的部分省略，两者都没有时为 `upstream error`），`SummaryFallback` 为真，绝不回退到完整 JSON。凭证事件的 `summary` 和凭证通知已经单独写出状态码和代码，这时不再重复，`summary` 为空。

| 类别 | 判断依据 |
| --- | --- |
| `suspended` 账号停用 | 错误代码为 `TOS_VIOLATION`、`account_deactivated` 等停用代码；或状态码为 400/401/403，且 message 说账号或组织被停用（“violation of Terms of Service”“account has been disabled”等，见 `suspendedPhrases`）。单纯的 403 不算：缺少权限（`PERMISSION_DENIED`）和项目未启用 API（`SERVICE_DISABLED`）也返回 403 |
| `auth` 凭证失效 | 状态码 401、`quota.ErrUnauthorized`，或 `invalid_grant`、`authentication_error` 等代码 |
| `rate_limited` 限流 | 状态码 429 或 `RESOURCE_EXHAUSTED`；Magpie 对锁定账号的本地回答（message 含 `X-Magpie-Account:`）除外，它们不是厂商的响应，由 `ignite.CooldownWait` 和 `ignite.Classify` 处理 |

账号的凭证问题记在 `state.json` 的 `credentials`（按 `auth_index`，`store.CredentialState`），代码在 `internal/engine/credential.go`：

- **点火发现**：自动点火在发出请求前读一次凭证指纹。读不到时，这次失败的指纹记为未知，不在失败后补读：请求在途时用户可能已经换了凭证，补读到的是没用过的新凭证。遇到 `auth` 或 `suspended` 时，`ignite.RecordCredentialFailure` 记下失败但不设重试时间，记录不带 `until`、带 `reason` 的 `ignite_paused` 事件，账号进入问题状态。之后定时点火和下次点火时间的计算跳过这个账号的所有额度组（`credentialBlocked`），状态页不显示下次点火时间，也不再每天重试。手动点火不受影响。
- **查询发现**：额度查询遇到 `suspended` 时立即进入问题状态。遇到 `auth` 时先计数（`query_reason`、`query_since`、`query_count`），连续至少 3 次、持续至少 30 分钟（`queryProblemCount`、`queryProblemAge`）才进入：access token 刚过期、Magpie 或代理 CLI 还没刷新时也可能返回 401，Lamplighter 不自己刷新这些 token，刷新的时间由它们决定，持续这么久的 401 才按凭证失效处理。已知过期的 token（`Expired`）不发给厂商，见“与 Magpie 的接口”的“额度记录”。其他结果清零计数。
- **通知**：进入问题状态时记录 `credential_invalid` 事件，并发送一次通知：`auth` 为“🔑 … 凭证失效”，`suspended` 为“⛔ … 账号已停用”（有申诉链接时附上，https 链接同时作为 Bark 的跳转链接）。同一账号已处于同一类问题时，点火和查询再发现都只更新指纹，不再记事件或通知；点火发现或以同一原因再次确认过的问题（例如查询发现、换凭证后点火又失败）记为点火确认（`ignited`），只有点火成功才能结束。之后查询发现另一种原因（例如停用后查询又连续 401 满 30 分钟）时，事件和通知按新原因和来源照常发出，点火确认继续保留；只有凭证指纹变化后，新原因的问题才不带点火确认；状态在 `state.json` 中，插件重启后也不重复。发送失败时随之后每轮查询重试，每轮最多发一次：查询发现问题的那一轮已经发过，轮末的 `flushCredentialNotices` 跳过本轮发过或试过的通知（`credNoticeTried`）；1 小时（`credentialNoticeAge`）后放弃；没有推送渠道时不发送，这个问题也就不发恢复通知。
- **凭证变化**：进入问题状态时记下凭证指纹（`host.Credential.Fingerprint`，由 `internal/magpie` 计算：refresh token 的 SHA-256 前 8 字节，没有 refresh token 时用 API key，再没有时用 access token）。每次主动查询读凭证文件时比较指纹，不同则标记 `changed`，点火按日程恢复；再次失败且原因相同时记下新指纹、重新暂停，不再通知。进入问题状态时读不到凭证文件，指纹记为空；之后第一次读到非空指纹也标记 `changed`，点火再试一次，失败后记下这次的指纹。这样不会因为没有指纹可比而永远暂停，重启后同样适用，代价是可能多一次点火尝试。Magpie 和代理 CLI 会改写凭证文件：每次刷新 token 都替换 access token，上游返回 TOS_VIOLATION 后还会写入 `tos_appeal_url`，所以不能用修改时间或整个文件判断重新登录。2026-11-17 在 Antigravity 凭证上实测，access token 每次刷新都变，refresh token 不变。每次刷新都换 refresh token 的服务，只有刷新成功时指纹才变，代价是每次刷新后多一次点火尝试。
- **恢复**：点火成功（含手动点火）结束任何凭证问题；额度查询成功只结束没有点火确认的问题。2026-11-16 Antigravity 账号因 TOS_VIOLATION 点火被拒后，额度查询仍然成功，查询成功不能说明停用已解除。结束时记录 `credential_restored` 事件；问题通知已送达时发送“✅ … 凭证已恢复”。

状态接口的账号带 `credential_problem`（`auth` 或 `suspended`），是 `state.json` 的副本，由 `engine.mu` 保护。凭证事件和它们的通知带同一个额度组 key（存在 `credentials` 的 `group` 中）：点火发现时是点火的额度组，其余是账号按显示顺序的第一个额度组（`accountGroup`），页面据此把推送挂到事件上。账号还没有额度组时两边都为空，页面改按标签匹配。凭证事件另外记下账号的 auth index（`Event.Account`，历史记录的 `a`），只供 MCP 找到账号，见“MCP”。

## 通知

`notify.Alerts.ProcessGroup` 比较每个窗口当前剩余额度和已通知等级：

- `config.Parse` 将阈值限幅到 0–100，依次约束 `low_threshold ≤ notice_threshold`、`critical_threshold ≤ low_threshold`；NaN 使用对应默认值，调整写入标准日志。有效配置始终满足 `notice ≥ low ≥ critical`。
- 等级为 normal、notice（≤50%）、low（≤20%）、critical（≤10%）、exhausted（≤0.01%）。等级变差时推送；critical 和 exhausted 使用 `timeSensitive`。
- 只有真正恢复才算等级变好：重置时间换到了新窗口，或者比上次读数高出 5 个百分点及以上（`RecoveryJump`）。主动查询和响应头的数值可能差 1 个百分点，同一窗口内的小幅回升不改变已通知等级，所以读数在阈值附近来回跳时只提醒一次。等级变好时只静默更新基线，否则下一轮的下降不会再提醒。
- 恢复通知和重置前提醒按窗口类别取 `recovery_notify`、`reset_reminder` 中的模式。`notify.SevenDayClass` 按窗口种类（`quota.Kind`）识别 5 小时和 7 天窗口；其他窗口在距离重置超过 1 天时被看到过（`state.json` 的 `long`）就算 7 天窗口，否则算 5 小时窗口。
- 恢复通知只看周期切换（`cycleEnded`）：两个重置时间都已知时，重置时间必须换到新周期，并且旧重置时间已过或额度回升了 `RecoveryJump`。Codex 未使用的窗口每次查询都返回向后顺延的重置时间，只看重置时间是否变化会把每次查询都当成重置。新读数没有重置时间时，旧重置时间已过或额度回升 `RecoveryJump` 即算切换。
- 用量记录在请求结束时才到达，重置前开始的长请求会在重置后带回上一周期的响应头。同一周期内早于当前读数的被动数据已在 `applyGroups` 丢弃，见“被动数据”。重置时间已过、并且属于已结束周期（`ended_reset`）或早于当前周期的读数直接忽略，否则会把状态拉回上一周期并再次触发恢复。
- **上游回跳**：重置之后，上游有时又返回一两次上一周期的读数（重置时间与 `ended_reset` 属于同一周期，`notify.Bounced`），且重置时间还没过，上一条规则拦不住。2026-11-14 07:00 Gemini 7 天窗口重置后，07:00:12 的读数回到旧周期的 94.76%，07:05 又回到新周期的 100%，旧代码因此推送了两次恢复、记录了两次 `reset` 事件。回跳的读数不更新通知基线，不推送下降、恢复或重置前提醒，`engine.recordQuotaTransitions` 也不据此记录 `reset` 或 `exhausted` 事件，下一次读数仍与回跳前的基线比较；历史样本照常写入。回跳的开始时间和次数记在窗口基线的 `bounce_since`、`bounce_readings` 中，读到非回跳的读数时清零。回跳持续 30 分钟（`bounceAcceptAge`）并且至少 3 次读数（`bounceAcceptReadings`）后，认为上游确实回到了旧周期，静默按回跳的读数重建基线并清空 `ended_reset`，不推送。已观察到的回跳都在 1–2 次读数、10 分钟内结束；读数次数的条件避免插件停止前后相隔很久的两次读数单独决定。2026-10-22 上游曾连续数小时报告新周期后回到旧周期，这种情况按上述规则在 30 分钟后接受。
- 检测到切换时记下待发送的恢复（`pending_recovery`），连同上一周期最后的读数和该周期是否用完（`exhausted`）。上一周期的读数取重置时间之前的最后一次读数（`cycle_reset`、`cycle_remaining`、`cycle_seen`），因为有的服务在重置后一段时间内仍返回旧的重置时间和新周期的数值。这个读数距离重置超过 15 分钟时不报告，用完的窗口到重置前都是 0，读数不会过期。上一周期重置前仍是 100% 时不推送：插件停止超过一个窗口后，Codex 未使用窗口顺延的重置时间看起来也已过期。`after_exhausted` 模式下只发送用完的周期。推送失败时下一次检查重试，1 小时后放弃。恢复与额度下降分别推送。
- `engine.probes` 为每个带重置时间的窗口（不论通知设置）在重置前和重置后 30 秒（`probeOffset`）各主动查询一次对应账号，每个账号每个时间点只查一次，相差不超过 10 秒的时间点视为同一个；这个时间点之后已有读数时跳过。重置后的查询只在重置后 10 分钟内（`probeLate`）进行，插件刚启动时页面上残留的旧重置时间等下一轮查询。`probes` 只计算，不做标记，主循环用它计算唤醒时间；只有 `runProbes` 在查询时记录。
- 重置前提醒：5 小时类窗口在重置前 1 小时提醒一次（`reset_notice_1h_for`），7 天类窗口在重置前 1 天提醒一次（`reset_notice_1d_for`）。`has_remaining` 模式只在剩余高于 `critical_threshold` 时提醒。
- 两个重置时间相差不超过 10 秒视为同一周期。
- 第一次看到某个窗口只记录基线，不推送。
- 推送失败（所有渠道都失败）时不更新已通知等级，下一次检查会重试。

Bark 请求为 `GET {bark_url}/{标题}/{正文}?group&level&icon&url`，标题和正文除 RFC 3986 非保留字符外全部百分号编码，返回 JSON 中 `code` 不为 200 视为失败。`icon` 默认使用仓库中的 `assets/logo.png`（`bark_icon`）。

### 渠道

`engine.applyRuntimeConfig` 在配置变化时按 `bark_url` 和 `webhook.url` 建立渠道，交给 `notify.Fanout`。`Fanout` 同时向所有渠道发送，等全部返回后：至少一个渠道成功就算送达，失败的渠道各记一条 `notify_failed` 事件，`error` 以渠道名开头，事件不带额度组 key，只带标签；所有渠道都失败才算推送失败，由调用方按上文的规则重试。只要有渠道成功就不重试，因为重试会让已经收到的渠道重复收到同一条通知。没有任何渠道时返回 `notify.ErrNotConfigured`，表示通知已关闭：额度提醒照常更新已通知等级、待发送的恢复和重置提醒，就像已经送达，这样之后再配置渠道时不会补发旧提醒；点火暂停和冷却提醒不记 `notify_failed`；Did Codex Reset 的记录不标为已见，配置渠道后仍可推送的照常推送。只配了 webhook 但设置无效时，`Fanout.Invalid` 让发送返回设置错误，按推送失败处理，并按上文的规则重试。

`notify.NewWebhook` 检查 webhook 设置：地址必须是 http 或 https，方法只能是 `GET`、`POST`、`PUT`，`GET` 不能带请求体，请求头名称必须合法且不区分大小写地不重复，请求头的值去掉首尾空白后不能含换行或控制字符（YAML 块标量末尾的换行会被去掉），占位符必须在 `notify.Placeholders` 中。检查不通过时 webhook 不启用，原因写进状态接口的 `notify_error`，页面显示为提示，测试推送也把它作为失败结果返回；配置的其他部分照常生效。

每条消息按位置转义占位符：地址中用与 Bark 相同的百分号编码，请求体按 `Content-Type` 做 JSON 字符串转义或表单编码，其他类型原样填入，请求头中把换行换成空格。没有写 `Content-Type` 时，把占位符换成一个单词后是合法 JSON 的请求体按 JSON 发送，其余按纯文本发送，所以 `[Lamplighter] {{text}}` 这样的文本不会被当成 JSON；请求体为空时发送内置 JSON（`source` 加上全部占位符，字段顺序固定）。`{{priority}}` 由 Bark 的级别换算：`timeSensitive` 为 `high`，其余为 `normal`；`{{kind}}` 取自 `Message.Kind`。

webhook 响应最多读 64 KB。非 2xx 视为失败；设置了 `success_json` 时，响应还必须是 JSON 对象，并且其中每个指定的顶层字段都等于期望值。期望值先经过一次 JSON 编码和解码再比较，所以 YAML 中的整数 `0` 与响应中的 `0.0` 相等。错误和测试结果中的响应文字会去掉 `*url.Error` 带的请求地址，并把地址、较长的路径段和查询参数值、请求头的值，以及 JSON 请求体中较长的字符串（例如 ntfy 的 topic）替换为 `[redacted]`，因为这些位置常带有 token。

通知文字在 `internal/notify/text.go` 中，中文和英文各一列，按 `state.json` 的 `language` 选择。语言由管理页面上报：推送时没有打开的页面，所以使用最近一次上报的语言，没有上报过时使用中文。窗口名在通知中始终写作 `5h`、`7d`。Did Codex Reset 的跳转链接按语言指向中文版或英文版的历史页面。

Did Codex Reset 每 `poll_seconds` 秒（最少 300 秒，对齐时间边界）读取最新 10 条记录。非 `manual:` 记录按 ID 去重，`manual:` 记录的 ID 可能变化，按内容去重；最多保留 100 个已见记录。第一次运行把现有记录标为已见，只在 `notify_current_pending` 开启时推送当前待生效的排期。

只推送仍有意义的记录，其余只标为已见：排期必须是 `scheduleState = pending` 且窗口尚未结束；完成记录的时间（`completedAt`、`effectiveAt`、`announcedAt` 中最晚的一个）不能早于 48 小时前。已兑现的排期会离开 `kind=all` 列表，失去关联后又会以 `elapsed` 回到列表，旧记录因此可能在任意时间出现。每条 X 帖子是一条记录，主帖和回复会给同一次排期各建一条。重置种类、窗口起点和范围（`scope.plans`、`scope.windows`）相同的排期视为同一次排期。只用起点比较，因为同一次排期的记录有的只有 `effectiveAt`，有的带完整的 `scheduleWindow`。与 Did Codex Reset 首页一致，同一次查询中只推送其中 `announcedAt` 最晚、且本身可推送的一条，时间相同时按记录键取一条，其余只标为已见。回复可能晚一两次查询才进入最新 10 条，所以推送过的排期连同帖子的 `announcedAt` 和通知内容（按 UTC 和英文生成）记在 `sent_events` 中，最多 100 个。之后出现的同一排期帖子，只有 `announcedAt` 更晚且内容（置信度、时间、范围）有变化时，才以“排期更新”为标题再推送；其余只标为已见。

排期时间取 `scheduleWindow`，没有时取 `effectiveAt`，按插件时区显示：起止相同时显示时刻，`scheduleConstraint = deadline` 时显示“终点 前”，其余（日期级排期）显示“起点～终点”。日期级窗口覆盖来源时区的一整天，起点通常早于公布时间，只显示起点会被误读为已经过去的时刻。

## 数据

数据目录的位置见“进程和数据目录”。

| 文件 | 内容 |
| --- | --- |
| `state.json` | `groups`：每个窗口的通知基线和最近一次成功主动查询确认的窗口集合；`scheduler`：每个额度组的点火状态；`codex_reset_updates`：已见记录和已推送的排期；`language`：通知语言；`plans`：各账号的原始套餐值、上次单独读取套餐的时间、上次主动查询的窗口、重读截止时间和待发送的套餐变化通知；`credentials`：各账号的凭证问题、指纹和通知状态（见“凭证问题”）。写入临时文件后替换 |
| `history/YYYY-MM-DD.jsonl` | 按 UTC 日期分文件的额度样本和事件 |
| `instance.lock` | 实例锁 |

历史记录每行一个 JSON：样本为 `{"k":"s","t":秒,"g":额度组,"w":窗口,"r":剩余,"x":重置,"s":"active|passive"}`，事件为 `{"k":"e","t":秒,"g":额度组,"e":类型,"v":级别,"m":文本,"l":标签,"d":详情,"p":参数,"a":账号}`，`a` 是账号的 auth index，只有凭证事件有。`m` 和 `d` 是写给日志的中文；`p` 是页面拼出事件文字所需的值：`ignite` 和 `ignite_manual` 为 `model`、`reset`，自动点火的 `ignite_failed` 为 `retry_seconds`、`error`、`summary` 和可选的 `cooldown`，手动点火的 `ignite_failed` 为 `manual`（`1`）、`error`、`summary`，`ignite_paused` 为 `until`、`error`、`summary` 和可选的 `reason`（`reason` 为 `auth` 或 `suspended` 时没有 `until`），`credential_invalid` 为 `reason`、`status`、`code`、`summary`、`appeal_url`、`source`（`ignite` 或 `query`），限流时还有 `until`，`credential_restored` 为 `source`，`model_refused` 为 `model`、`fallback`、`hours`，`notify` 为 `title`、`notify_kind`，`notify_failed` 为 `title`、`error`、`notify_kind`（`quota`、`recovery`、`reminder`、`cooldown`、`plan`、`circuit`、`credential`、`suspended`、`rate_limited`、`credential_ok`、`codex_reset`），恢复通知还有 `windows`（以逗号连接的窗口 ID），`plan_change` 为 `from`、`to`（套餐名），`codex_reset` 为 `count`；`reset` 和 `exhausted` 为 `window`、`remaining`、`kind`（`5h`、`7d` 或空）、`label`（窗口短名）及可选的 `reset`；时间为 RFC 3339 UTC。主动样本每次都写；被动样本只在数值或重置时间变化时写，同一窗口每分钟最多一次，期间的新值暂存在 `pendingSamp`，满一分钟后写入。历史接口对超过 2 天的范围分段，每段取最后一个样本：超过 2 天按 10 分钟，超过 4 天按 15 分钟，超过 8 天按 30 分钟，超过 15 天按 1 小时，使这些范围的每个系列不超过约 800 个点（图表约 1000 像素宽）。2 天及以内不分段。分段长度在响应的 `bucket_seconds` 中。每个系列还带有 `resets: [[t, x], ...]`，两个值均为 Unix 秒，表示从样本时间 `t` 起预定重置时间为 `x`。此数组在点降采样之前按原始样本生成，按 `t` 升序；相邻原始样本的非零 `x` 相差超过 60 秒才记为变化，已知与未知之间的切换始终记录，未知使用 `x=0`。范围前生效项使用范围开始前时间最大的样本的 `[t, x]`，其 `t` 可以早于样本的一天前导范围，不要求是该 `x` 首次出现的时间；整个系列从未有重置时间时返回空数组。`points` 仍为 `[t, remaining, passive]`，分段规则独立于重置事件。每个系列带有 `label`（含账号后缀，例如 `ChatGPT#rk`）和 `source_label`（不含后缀），页面按服务和 `source_label` 合并同一额度的多个账号。

管理历史查询通过 `store.History.QueryWindow` 单遍扫描保留记录。每个系列在范围外只保留时间最大的一个前驱样本，以及是否出现过非零重置时间的标记；同一时间的前驱取最后补写值，乱序补写不会覆盖较新的前驱。范围内记录收集后按时间稳定排序，再计算重置变化、点降采样和事件。前驱在一天前导范围内时也作为 `points` 的起始点，更早的前驱只提供重置状态；范围内没有样本的系列不输出。内存为 `O(系列数 + 范围内记录数)`，不对范围外记录排序，不创建磁盘临时文件，不依赖临时目录。扫描循环和范围内处理保留 context 取消检查。

额度组 key 为 `<服务>:<auth_index>:<分组>`，例如 `codex:3:codex:main`、`claude:3:claude:seven-day-fable`、`antigravity:4:antigravity:gemini-models`。它同时是点火目标 ID。

## 设计约束

- 点火只经 Magpie 网关发送，并用 `X-Magpie-Account` 锁定到具体账号；不要直接向服务发送模型请求。
- 额度请求的出口必须与 Magpie 对同一账号的请求一致；出口无法确定时跳过请求，不能退回直连。
- 不在日志、事件、状态接口或磁盘中输出 access token、`bark_url`、`webhook` 的地址、请求头和请求体，以及 `admin_key`、`mcp_key`。
- Magpie 的接口和文件格式以 Magpie 最新源码为准；格式与预期不符时返回 `host.ErrFormat`，不猜测字段。
- 修改 `state.json` 结构时保持向后兼容：新字段使用缺省值，旧字段缺失时继续运行。
