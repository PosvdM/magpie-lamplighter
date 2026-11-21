# 交接：移植到 Magpie（2026-11-21）

## 状态

- 方案已获用户同意，可以直接开工，做完后按“交付”一节合并和发版，中间不用再问用户。
- 第一阶段（删除 CPA 专属代码、定义 `host.Host` 接口）和第二阶段（五项任务，见“第二阶段的结果”）都已完成并合并到 `dev`，`go vet ./...`、`go test -race ./...`、`node --test internal/web/page_test.cjs`、`node --test npm/test/` 都通过。`main` 停在 `0606041`（复制自 cpa-plugin-lamplighter v0.8.6），开发期间不动。
- 下一步是 Docker 端到端联调，见“合并和联调”的第 2 步。

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
- npm 包名 `magpie-lamplighter`，页面显示名 Magpie-Lamplighter，版本从 0.1.0 开始，用 Trusted Publishing 发布（参照 PosvdM/pi-endpoints 的 `.github/workflows/publish.yml`）。第一次发包由用户在本机执行 `npm publish`，步骤见“交付”。
- MCP 有两种接法：对外监听的 `/mcp`，以及 Magpie 的 tool 方式（可选）：地址是 `/mcp/magpie/tools/magpie-lamplighter`，工具名带 `magpie-lamplighter_` 前缀（如 `magpie-lamplighter_list_quotas`）。Magpie 用插件的短名称命名，短名称是 npm 包名去掉 `opencode-`、`tools-`、`page-` 前缀。

接口、数据目录和 HTTP 路由见 [架构](../architecture.md) 的“进程和数据目录”“与 Magpie 的接口”“HTTP 接口”三节。

## 第二阶段的结果

五项任务各在一个工作树中开发，已按 D（对外监听和鉴权）、C（npm 外壳和发布）、A（账号、凭证和额度记录）、B（网关点火和预热冲突）、E（前端）的顺序合并到 `dev`。合并后的收尾：统一访问网关的辅助函数（`internal/magpie/client.go`），`magpie.Host` 的运行状态改为字段；手动刷新时让 Magpie 刷新凭证已过期账号的额度；`magpie_warmup` 只列出 Lamplighter 会点火的账号；`host.Account` 增加 Magpie 的账号名 `User`，Codex Team 席位的预热时间点按它匹配；清理 CPA 版遗留的说明。各部分的现状见 [架构](../architecture.md) 和 [开发](../development.md)。

## 合并和联调

1. 各任务合并回 `dev`，测试全部通过（已完成）。
2. 在测试服务器的 Magpie Docker 中安装 dev 版本做端到端验证：账号列表、三种服务的主动查询和额度记录、页面嵌入和独立访问、两种 MCP、tool 调用。先关闭 `ignition.enabled`，确认无误后对一个账号手动点火。服务器的地址和登录方式由用户另行提供。以下几点目前只按 Magpie 源码和文档实现，要实测：
   - **程序的启动和退出**：Bun 中 `node:child_process` 经 Magpie 包装的 `Bun.spawn` 能否正常启动程序；Magpie Restart（安装、更新、设置插件）和宿主退出时程序是否随之结束，不残留进程；端口被旧进程占用时 `Supervisor` 的重试。
   - **页面加载**：在 Magpie 桌面窗口和 `magpie web` 两种方式下都能加载数据，即页面令牌、`Origin: null` 的 CORS 和 Host 检查都放行 Magpie 的请求（`magpie web` 的页面服务代理是否把 Host 设为 `127.0.0.1:<端口>`）。
   - **502 和出错页**：页面 10 秒内没有发出 `hello` 时，Magpie 是否把服务未就绪（502）期间的页面标为出错。
   - **主题、语言和截图**：主题和语言随 Magpie 切换；截图经 `image.copy` 能复制到剪贴板（桌面窗口和 `magpie web` 都试）；沙箱中复制文字的退路。
   - **独立访问**：经对外监听打开页面，收到 401 后输入管理密钥的流程；`status.public` 的各字段和设置页的显示；`-listen` 绑定非回环地址后从别处访问得到 403。
   - **预热提示**：在 Magpie 中开启 Codex 或 Claude 的预热后，`magpie_warmup` 和页面提示是否正确；关闭点火或取消勾选额度组后提示是否消失；Codex Team 席位账号自己的时间点（`codexWarmAtOf`）能否匹配。
   - **账号对应**：`/v1/magpie/accounts` 的 pin 与 `logins.json` 中的 `id`、插件账号按 `nameID` 算出的 id 能否对上真实的 Magpie；额度记录（`kept=1`）的额度卡能否按 `user` 对应到账号，组 key 和窗口 ID 是否与主动查询一致。
   - **手动刷新过期凭证**：闲置的 Claude 账号 token 过期后，点“刷新”或调用 `refresh_quotas` 是否经 `POST /v1/magpie/quotas/refresh` 拿到新读数，经 Magpie 的 tool 调用能否在 50 秒内返回。
   - **Antigravity 的 User-Agent**：额度查询用 `internal/providers` 中的 Hub 格式（`antigravity/hub/<版本> darwin/arm64`），agy-cli 插件自己用 `antigravity/cli/<版本> (aidev_client; …; auth_method=…)`。确认 Hub 格式能拿到 Gemini 额度组，且不会因与 agy 的 UA 不同而增加风险。
   - **Claude 点火**：经 Claude Code 转发的点火能否开始 5 小时窗口，`X-Magpie-Account` 在这条路径上是否生效，额度响应头（`anthropic-ratelimit-unified-*`）是否及时写回；Claude Code 启动慢时 2 分钟的点火时限是否够用。
   - **Codex 点火**：网关是否把 `x-codex-*` 响应头转发给客户端；不转发时只能靠“确认”中的主动查询。
   - **网关的错误格式**：锁定账号被拒（400 套餐不含该模型、404 没有账号提供该模型、429 账号休息中）时，`gateway_http_error` 的正文能否被 `ignite.Classify` 和 `failure.Diagnose` 正确分类。模型列表中 Antigravity 模型的前缀是否为 `agy-cli/`。
   - **Docker 中的路径**：`XDG_STATE_HOME`（数据目录应为 `/config/state/lamplighter`）、`MAGPIE_CONFIG_DIR`（缺省为宿主的工作目录）和 agy-cli 的 token 文件（`$XDG_STATE_HOME/magpie-agy-bridge/accounts/<agyHome>/…`；第一个账号在 `~/.gemini/antigravity-cli/`）的实际位置；升级插件后数据是否保留。
   - **端口冲突**：内部监听默认 `127.0.0.1:8317`，与 CPA 的默认端口相同；对外监听默认 `127.0.0.1:8318`，与 CPA 版 Lamplighter 的 MCP 端口相同。同一台机器还运行 CPA 时要改 Magpie 插件选项中的 `port` 或 `public_listen`，README 中要写明。
3. GPT 审查 `main...dev` 的差异，修完后开 PR 到 `main`。

## 已知限制

v0.1 有以下限制，README 改写时写进去：

- macOS 和 Windows 上 agy 自己登录的第一个 Antigravity 账号，凭证在系统钥匙串中，Lamplighter 不读取，返回明确的错误；在 Magpie 中再添加一次该账号即可。
- `auth_method` 不是 `consumer`（或为空）的 Google 账号要另行查询 Cloud Code project，v0.1 返回错误。
- `MAGPIE_ADDR` 设为局域网地址时网关要求网关密钥，v0.1 不支持。
- `-listen` 绑定非回环地址后，从其他机器访问内部监听会得到 403；从外部访问要用对外监听（`public_listen`）。

## 交付

1. 改写 README 和 README_EN（删去顶部的开发中说明，按 Magpie 版写安装、配置和使用，包括“已知限制”和端口冲突的说明）。
2. PR 合并到 `main`，删除本交接文档。
3. 首次发布 0.1.0：npm 只能为已存在的包配置 Trusted Publisher，所以第一个版本由用户在本机发布：
   1. 在合并后的 `main` 上，登录 npm 后运行 `sh scripts/build.sh && npm publish`（`package.json` 的版本已是 `0.1.0`）。
   2. 在 npm 上为 `magpie-lamplighter` 配置 Trusted Publisher：仓库 `PosvdM/magpie-lamplighter`，文件 `release.yml`。
   3. 打 `v0.1.0` 标签并推送。`release.yml` 的 `publish` 发现 0.1.0 已在 npm 上，跳过发布，`release` 照常创建草稿 Release。
   4. `release.yml` 创建的是草稿 Release，补充说明后手动发布；程序按 GitHub 的最新 Release 提示更新，草稿发布前不会提示。

   之后的版本：修改 `package.json` 的版本号，打 `v<版本号>` 标签推送，CI 测试、构建、经 Trusted Publishing 发布并创建草稿 Release，见 [开发](../development.md) 的“发布”。
4. cpa-plugin-lamplighter 保持不动。它的 README 顶部说明和 issue #34 的回复，等用户用过 Magpie 版之后再做，评论稿先给用户看。

## 注意事项

- README 和 README_EN 仍是 CPA 版的说明，发版前统一改写。
- `internal/engine/fakehost_test.go` 中的 `applyHeaders` 等辅助函数模拟点火响应头，测试仍在用。
- 点火会发送真实请求并开始新的 5 小时窗口，联调时不要对生产账号批量点火。
- 五个工作树（`.worktrees/sa-*`）和 `subagent/*` 分支还保留着，验收后再清理。工作树中有指向主检出 `node_modules` 的软链接，提交时按文件添加，不要用 `git add -A`。
