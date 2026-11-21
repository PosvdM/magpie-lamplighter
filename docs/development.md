# 开发

## 环境

- Go 1.26。程序是纯 Go，不需要 cgo，可以在 Windows、macOS 和 Linux 上构建和测试。
- Node.js，用于检查 `internal/web/page.js` 的语法、运行页面和 npm 外壳的测试，以及打 npm 包。外壳在 Magpie 中由 Bun 运行，但只用 Node.js 也有的模块，本地不需要安装 Bun。

## 测试

```bash
go vet ./...
go test ./...
go test -race ./...
node --check internal/web/page.js
node --test internal/web/page_test.cjs
node --test npm/test/
```

测试使用假的 `host.Host` 和 HTTP 服务器，不会发送真实请求。`npm/test/program.test.mjs` 会编译 Go 程序（没有 Go 时跳过），由外壳启动它并调用一个 tool；程序没有版本号，不会向 GitHub 查询新版本，`MAGPIE_ADDR` 指向回环地址上一个关闭的端口。

推送到 `main`、`dev` 和 PR 时，GitHub Actions（`.github/workflows/ci.yml`）在 Linux 上检查 `gofmt -l .` 的输出为空，并运行 vet、测试、Node.js 语法检查和 npm 外壳的测试；race 检查和页面测试用于本地验证，发布时也会运行。只改动 Markdown、`docs/` 或 `assets/` 的提交不触发。同一分支的新提交会取消仍在运行的旧任务。

## 本地运行

```bash
go run . -listen 127.0.0.1:8317 -data-dir /tmp/lamplighter
```

然后打开 `http://127.0.0.1:8317/`。程序通过 `MAGPIE_ADDR` 找到 Magpie 网关，未设置时无法读取账号。

## 构建

```bash
sh scripts/build.sh [版本号]
```

脚本用 `CGO_ENABLED=0` 静态编译 linux、darwin、windows 各 amd64 和 arm64 六个平台的程序，放在 `dist/bin/<GOOS>-<GOARCH>/lamplighter`（Windows 为 `lamplighter.exe`），并把版本号写入 `dist/bin/VERSION`。版本号默认取 `package.json` 的 `version`，经 `-ldflags "-X main.version=…"` 写入程序，`lamplighter -version` 可以查看。程序是纯 Go，在任何一个平台上都能交叉编译全部六个平台。`dist/` 已在 `.gitignore` 中，构建产物不提交。

`npm pack` 和 `npm publish` 先运行 `prepack`（`scripts/check-dist.mjs`），确认六个平台的程序都在、非空、有执行权限，且 `dist/bin/VERSION` 与 `package.json` 的版本号相同，否则中止。包中的文件由 `package.json` 的 `files` 列出：`npm/` 下的入口和页面（不含 `npm/test/`）、`dist/bin/`，以及 npm 自动加入的 `README.md`、`LICENSE`、`package.json`。压缩后约 22 MB，解压后约 55 MB。用 `npm pack --dry-run` 可以查看包的内容和大小。

外壳的运行方式见[架构](./architecture.md#插件外壳)。

## 发布

1. 修改 `package.json` 的 `version`，提交。
2. 在这个提交上打标签 `v<版本号>` 并推送。
3. `.github/workflows/release.yml` 依次运行：
   - `test`：检查标签与 `package.json` 的版本号一致，运行 gofmt 检查、`go vet ./...`、`go test -race ./...`、页面和 npm 外壳的测试。
   - `build`：`sh scripts/build.sh <版本号>` 构建六个平台的程序，`npm pack` 打包。
   - `publish`：用 npm Trusted Publishing（OIDC，不需要令牌）发布这个包，需要 npm 11.5.1 或更高版本。
   - `release`：创建 GitHub Release 的草稿，附上包和 `checksums.txt`。
4. 在 GitHub 上补充草稿的说明后发布。程序通过 GitHub 的最新 Release 判断是否有新版本（`internal/release`），草稿发布前不会提示更新。

npm 上的 Trusted Publisher 设置要写仓库 `PosvdM/magpie-lamplighter` 和文件名 `release.yml`；重命名这个文件时，要在 npm 上删除旧设置并重新创建。npm 只能为已存在的包配置 Trusted Publishing，所以第一个版本在本机发布：登录 npm 后运行 `sh scripts/build.sh && npm publish`。

## 修改约定

- 读取 Magpie 的内部文件（如 `logins.json`）时，格式与预期不符要返回包装 `host.ErrFormat` 的错误，不猜测字段；对照 Magpie 最新源码中的 `internal/provider` 编写。
- 不刷新 Claude 和 Codex 的 token，原因见[与 Magpie 的接口](./architecture.md#与-magpie-的接口)。
- 增加、删除 MCP 工具或修改工具参数时，同步修改 `npm/tools.js`；修改 `main.go` 中 `-listen` 的默认端口时，同步修改 `npm/lamplighter.js`、`npm/page/open.js` 和 `package.json` 的 `magpie` 字段。`npm/test/` 中的测试会检查这两处是否一致。
- Bun 入口传给程序的环境变量（`MAGPIE_CONFIG_DIR`，以及从 `MAGPIE_*_PROXY` 映射回来的代理）及其原因见[插件外壳](./architecture.md#插件外壳)；修改前对照 Magpie 源码 `internal/plugin/host.go` 的 `hostEnv` 和 `host.js` 的 `proxied`。
- 修改额度请求的地址或请求头时，更新[架构文档](./architecture.md)中的表格。
- 修改轮询、跳过规则、点火时间或失败保护时，同步更新测试、README 和架构文档。
- 后台循环中的 goroutine 必须有 `recover`，取消监视器必须在调用结束前退出。HTTP 请求设置超时。
- 修改页面时保持 HTML、CSS、JS 分文件；文字插入 DOM 时使用 `textContent`。新增或修改页面文字时，同时修改 `page.js` 中 `I18N` 的中文和英文词条；新增或修改推送文字时，同时修改 `internal/notify/text.go` 的两列。增加一种语言时，两处各加一份。图表颜色只表示剩余额度的高低，使用与额度条相同的三档变量。
- 用户可见的行为、配置和限制写进 README（中英文同步），实现细节写进 `docs/`。

## 真实环境验证

点火会发送真实模型请求并开始新的 5 小时窗口。首次接入真实的 Magpie 时，先把 `ignition.enabled` 设为 `false`，确认额度、被动数据、页面和通知正常后再开启。接管点火前关闭 Magpie 自带的预热，以免重复点火。
