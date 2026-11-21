# 交接：移植到 Magpie（2026-11-21）

## 状态

- 方案已获用户同意，可以直接开工，做完后按“交付”一节合并和发版，中间不用再问用户。
- 第一阶段（删除 CPA 专属代码、定义 `host.Host` 接口）和第二阶段（五项任务，见“第二阶段的结果”）都已完成并合并到 `dev`，`go vet ./...`、`go test -race ./...`、`node --test internal/web/page_test.cjs`、`node --test npm/test/` 都通过。`main` 停在 `0606041`（复制自 cpa-plugin-lamplighter v0.8.6），开发期间不动。
- Docker 端到端联调已完成，结论和仍未实测的项见“合并和联调”的第 2 步。下一步是第 3 步。

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
2. 在测试服务器的 Magpie Docker（v0.1.4634）中安装了 dev 版本 `0.1.0-dev.fd766b7`（把包解压到配置卷中的文件夹，`magpie plugin add <文件夹>`），`ignition.enabled` 保持关闭，只对一个 Codex 账号手动点火一次（已完成）。没有发现要改代码的问题。结论：
   - **通过**：程序的启动和退出（Restart 后旧宿主和程序都结束，端口被占用时按 1、2、4、8、16 秒重试并恢复）；`magpie web` 下的页面加载、令牌、CORS 和 Host 检查；服务未就绪时 Magpie 显示 502 页并每 2 秒重试，没有把页面标为出错，服务恢复后自动加载；语言跟随 Magpie；`magpie web` 下截图经 `image.copy` 进入剪贴板，沙箱中复制文字的退路可用；对外监听的 401、`WWW-Authenticate`、两把密钥互不通用、`/mcp` 的 Origin 校验和 `status.public` 各字段；`-listen 0.0.0.0` 时从容器 IP 访问得到 403；`magpie_warmup`（含 `codexWarmAtOf` 的设置和 `off`、取消勾选额度组、关闭点火）；pin 与 `logins.json` 的 `id` 及 agy-cli 账号按 `nameID` 算出的 id 一致，额度记录按 `user` 对应到账号，组 key 和窗口 ID 与主动查询一致；凭证过期时改读额度记录，手动刷新经 `POST /v1/magpie/quotas/refresh` 拿到新读数，tool 方式的 `refresh_quotas` 约 1 秒返回；Antigravity 用 Hub 格式的 User-Agent 拿到 Gemini 和 Claude / GPT 两个额度组；网关错误正文（404 锁定账号不提供该模型、404 未知模型）被正确分类，Antigravity 模型的前缀为 `agy-cli/`；数据目录为 `/config/state/lamplighter`，`MAGPIE_CONFIG_DIR` 为 `/config/magpie`，容器重建后插件和数据都保留；Codex 点火带 `X-Magpie-Account`，由锁定的账号完成，约 1.2 秒读完流。
   - **结论与预期不同**：网关不转发 `x-codex-*` 响应头，Codex 点火由主动查询确认（已写入架构文档的“确认”）。v0.1.4479 的 Magpie 没有 `GET /v1/magpie/accounts`，读不到账号；README 改写时写明 Magpie 的最低版本。
   - **未实测**：Magpie 桌面窗口中的页面加载、主题和截图（服务器没有桌面）；Magpie 主题设置的切换（不改用户的 Magpie 设置，只验证了“跟随系统”时随浏览器切换）；在浏览器中经对外监听输入管理密钥的流程（对外监听只在容器回环地址上，只用 HTTP 请求验证了 401）；Claude 点火能否开始 5 小时窗口、Claude Code 启动慢时 2 分钟是否够用（测试时两个 Claude 账号额度将尽，且 Magpie 的预热会开启窗口）；Antigravity 点火（账号曾被封，不发点火请求）；400 套餐不含该模型和 429 账号休息中的真实响应（只按 Magpie 源码中的文字验证了分类）；从 npm 升级插件后数据是否保留。
   - 端口冲突仍要在 README 中说明：内部监听默认 `127.0.0.1:8317`，对外监听默认 `127.0.0.1:8318`，与 CPA 和 CPA 版 Lamplighter 的默认端口相同；Docker 中 Magpie 与它们不在同一个网络命名空间时不冲突。
3. GPT 审查 `main...dev` 的差异，修完后开 PR 到 `main`。

## 已知限制

v0.1 有以下限制，README 改写时写进去：

- macOS 和 Windows 上 agy 自己登录的第一个 Antigravity 账号，凭证在系统钥匙串中，Lamplighter 不读取，返回明确的错误；在 Magpie 中再添加一次该账号即可。
- `auth_method` 不是 `consumer`（或为空）的 Google 账号要另行查询 Cloud Code project，v0.1 返回错误。
- `MAGPIE_ADDR` 设为局域网地址时网关要求网关密钥，v0.1 不支持。
- `-listen` 只能是回环地址，其他地址时程序报错退出；从其他机器访问要用对外监听（`public_listen`）。

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
