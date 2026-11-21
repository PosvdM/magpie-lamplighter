# 交接：移植到 Magpie（2026-11-21）

## 状态

- 方案已获用户同意，可以直接开工，做完后按“交付”一节合并和发版，中间不用再问用户。
- 第一阶段已完成，提交在 `dev` 分支：删除了 CPA 专属代码，定义了 `host.Host` 接口，`internal/magpie` 是返回 `host.ErrNotImplemented` 的桩，`go test ./...` 和页面测试都通过。`main` 停在 `0606041`（复制自 cpa-plugin-lamplighter v0.8.6），开发期间不动。
- 下一步是第二阶段：按下文的五项任务并行开发，每项一个工作树，从 `dev` 拉分支，完成后合并回 `dev`。

## 已确定的决策

- 只支持 Magpie，不做宿主抽象。v0.1 只支持 Codex、Claude 和 Antigravity（Gemini）。Kimi 和 Grok 的代码已删除，以后从 cpa-plugin-lamplighter 搬回。
- 额度直接向厂商查询，不经过 Claude Code。三种服务都经 Magpie 网关点火，用请求头 `X-Magpie-Account: <pin>` 锁定账号。
- 被动数据每 60 秒读一次 `GET /v1/magpie/quotas?kept=1`。Magpie 给插件的用量记录没有响应头。
- 内部监听只绑定回环地址，不要密钥，供 Magpie 页面和 tool 使用；可选的对外监听上，页面和接口要 `admin_key`，`/mcp` 要 `mcp_key`。
- 账号名称保存在 Lamplighter 配置的 `account_names` 中：Magpie 没有让插件修改账号名称的接口。
- Claude 和 Codex 的 token 只读不刷新，刷新会更换 refresh token，让 Magpie 或代理 CLI 掉线。
- 读取 Magpie 内部文件时，格式对不上返回包装 `host.ErrFormat` 的错误，不猜测字段。不追 Magpie 的版本变化，开发时拉 Magpie 最新源码（GitHub `yetone/magpie`）对照。
- Magpie 容器保持 UTC，点火时间由 Lamplighter 按配置的 `timezone` 换算。
- 不迁移 CPA 版的历史数据。后台常驻（setsid）暂不做，等上游 yetone/magpie#7114 的答复。
- npm 包名 `magpie-lamplighter`，页面显示名 Magpie-Lamplighter，版本从 0.1.0 开始，用 Trusted Publishing 发布（参照 PosvdM/pi-endpoints 的 `.github/workflows/publish.yml`）。第一次发包可能要用户在本机手动执行 `npm publish`。
- MCP 有两种接法：对外监听的 `/mcp`，以及 Magpie 的 `/mcp/magpie/tools/lamplighter`（可选）。

接口、数据目录和 HTTP 路由见 [架构](../architecture.md) 的“进程和数据目录”“与 Magpie 的接口”“HTTP 接口”三节。

## 第二阶段任务

开发按 sister-squad：前端、后端和联调用 Claude，审查用 GPT；额度用完不重试，停下来等重置。每项任务只改自己列出的文件，需要改别处时先和协调者确认。每项都要补测试，并更新架构文档中自己负责的部分；README 在发版前统一改写，任务中不改。

### A：账号、凭证和额度记录

- 文件：`internal/magpie/accounts.go` 及新建的同目录文件（凭证、代理、额度记录各自成文件）。
- 实现 `Accounts`、`Credential`、`KeptQuotas`、`RefreshQuota`：
  - `Accounts` 读网关的 `GET /v1/magpie/accounts`（Magpie 源码 `internal/gateway/accounts_list.go`）。
  - `Credential` 读 Magpie 配置目录中的 `logins.json`（Codex、Claude）和 agy-cli 插件的账号文件（Antigravity），以及 `providers.json`、`settings.json` 中的代理。按 Magpie 源码 `internal/provider` 确认格式和代理的优先级。`Fingerprint` 的算法见架构文档“凭证问题”；token 过期且不能刷新时设 `Expired`。Google 的 token 可以在内存中刷新。
  - `KeptQuotas` 把 Magpie 的额度记录转成 `quota.RawGroup`，组 key 和窗口 ID 必须与 `internal/providers` 中该服务 `Fetch` 的结果相同，否则不会更新已有窗口。
  - `RefreshQuota` 调用 `POST /v1/magpie/quotas/refresh`。Claude 的刷新会运行 Claude Code 的 `/usage`，所以要限频。
- 待定：`engine.shouldSkipActive` 目前只对有响应头解析的服务（Codex、Claude）生效。Antigravity 有了额度记录后是否也跳过主动查询，由这项任务提出方案，经协调者确认后修改 `internal/engine/poll.go`。
- 测试用从 Magpie 源码或测试数据中整理的样例文件，格式错误的样例要得到 `host.ErrFormat`。

### B：网关点火和预热冲突

- 文件：`internal/magpie/gateway.go` 及同目录新文件、`internal/ignite`、`internal/failure`、`internal/engine/stream*.go`、`internal/engine/fire.go`。
- 实现 `Models`（`GET /v1/models`）和 `OpenModelStream`、`ReadModelStream`、`CloseModelStream`：按 `Protocol` 分别发到 `/v1/responses`、`/v1/messages`、`/v1/chat/completions`，带 `X-Magpie-Account`，`CloseModelStream` 取消请求。`StreamChunk.Payload` 的格式要符合 `internal/engine/stream_parse.go` 的解析规则；网关的格式不同时修改解析器。
- 网关的错误转成 `host.Error`，对照 Magpie 的错误格式检查 `ignite.Classify`、`ignite.ModelRejected`、`failure.Diagnose`。CPA 特有的分类（本地冷却 `model_cooldown`、`auth_not_found`、`unknown provider for model`）在 Magpie 中没有对应时删除，有对应时改成 Magpie 的写法。
- Claude 在 Magpie 中可能经 Claude Code 转发（Magpie 文档 `docs/subsystems/claude-subscription-bridge.md`），确认点火请求能开始 5 小时窗口，以及 `X-Magpie-Account` 在这条路径上有效。
- 预热冲突：Magpie 自带的预热和 Lamplighter 的点火同时开启会重复点火。查明 Magpie 的预热设置，在状态接口中加顶层字段 `magpie_warmup`：开启了预热的账号 pin 列表，没有时为空数组。页面的提示由任务 E 实现。
- 改写架构文档的“请求”“流式执行与取消”“调用日志”“模型选择”“失败分类和保护”各节，删除其中剩下的 CPA 内容。

### C：npm 外壳和发布

- 文件：`package.json`、`npm/`（新建：Bun 入口、页面启动文件）、`.github/workflows/`、`scripts/`。
- 插件声明：按 Magpie 文档 `docs/subsystems/plugin-runtime.md` 和 `plugin-tools.md` 写 `magpie` 字段：
  - 页面 `magpie.page`，`service` 指向内部监听的端口选项。页面文件只负责跳到 `magpie.service`，没有时跳到 `http://127.0.0.1:<端口>/`（参照 `@magpie-community/page-chat`），管理页面本身由 Go 程序提供。
  - tools 与 `internal/mcp` 中的工具一一对应，调用内部监听的 `POST /api/tools/{name}`，Magpie 的 tool 超时为 55 秒。
- Bun 入口按平台启动随包发布的 Go 程序（linux amd64/arm64、darwin amd64/arm64、windows amd64），传 `-listen 127.0.0.1:<端口>`，`MAGPIE_ADDR` 原样传给子进程；宿主退出时子进程随之退出。
- 发布：推送 `v*` 标签后先测试，再构建各平台程序、打 npm 包，用 Trusted Publishing 发布，并创建 GitHub Release。版本号经 `-ldflags "-X main.version=..."` 写入程序。
- 更新开发文档的“构建”“发布”两节。

### D：对外监听和鉴权

- 文件：`internal/server`、`internal/mcp`、`internal/config`、`main.go`。
- `public_listen` 上：`web_enabled` 为真且 `admin_key` 非空时提供页面和 `/api/*`，要求 `Authorization: Bearer <admin_key>`，错误时返回 401；`mcp_enabled` 时提供 `/mcp`，沿用 `mcp_key` 和现有的 Origin 校验。两者都关闭时不监听。内部监听保持不要密钥。
- 修改配置时按现有的 `mcp.Configure` 方式只在必要时重新绑定；密钥比较用常数时间。
- 更新架构文档的“HTTP 接口”和“MCP”两节。

### E：前端

- 文件：`internal/web/*`。
- 嵌在 Magpie 中时：页面经 Magpie 的页面服务反向代理打开，在沙箱 iframe 中运行（Magpie 文档 `plugin-runtime.md` 的页面一节，源码 `internal/gui/pagebridge.js`、`pageservice.go`）。查明主题和语言能否从 Magpie 获得；能获得时跟随，不能时用浏览器的设置。删除读取 CPA 管理中心父页面的代码（`parentRoot`、`watchParent` 等）。
- 独立访问时：遇到 401 输入管理密钥，密钥保存在 `localStorage`（已实现，确认在两种方式下都正常）。打码开关按浏览器保存（已实现）。
- 设置页增加对外监听的设置：`public_listen`（已有输入框）、`web_enabled`、`admin_key`（可生成，与 `mcp_key` 的做法一致）。
- `status.magpie_warmup` 非空时显示提示，说明哪些账号开启了 Magpie 的预热，需要关闭以免重复点火。
- `internal/web/page.css` 中 Kimi 和 Grok 的颜色变量可以保留。
- 更新架构文档的“管理页面”一节。

## 合并和联调

1. 各任务合并回 `dev`，解决冲突后 `go vet ./...`、`go test -race ./...`、`node --test internal/web/page_test.cjs` 全部通过。
2. 在测试服务器的 Magpie Docker 中安装 dev 版本做端到端验证：账号列表、三种服务的主动查询和额度记录、页面嵌入和独立访问、两种 MCP、tool 调用。先关闭 `ignition.enabled`，确认无误后对一个账号手动点火。服务器的地址和登录方式由用户另行提供。
3. GPT 审查 `main...dev` 的差异，修完后开 PR 到 `main`。

## 交付

1. 改写 README 和 README_EN（删去顶部的开发中说明，按 Magpie 版写安装、配置和使用）。
2. PR 合并到 `main`，删除本交接文档。
3. 打 `v0.1.0` 标签发布。Trusted Publishing 还没配置好时，请用户在本机执行 `npm publish`。
4. cpa-plugin-lamplighter 保持不动。它的 README 顶部说明和 issue #34 的回复，等用户用过 Magpie 版之后再做，评论稿先给用户看。

## 注意事项

- 架构文档中“请求”以后的几节还有 CPA 的内容，由任务 B 改写；README 仍是 CPA 版的说明。
- `internal/engine/fakehost_test.go` 中的 `applyHeaders` 等辅助函数模拟点火响应头，测试仍在用。
- 点火会发送真实请求并开始新的 5 小时窗口，联调时不要对生产账号批量点火。
