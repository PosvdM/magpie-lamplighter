# 开发

## 环境

- Go 1.26。程序是纯 Go，不需要 cgo，可以在 Windows、macOS 和 Linux 上构建和测试。
- Node.js，用于检查 `internal/web/page.js` 的语法和运行页面测试。

## 测试

```bash
go vet ./...
go test ./...
go test -race ./...
node --check internal/web/page.js
node --test internal/web/page_test.cjs
```

测试使用假的 `host.Host` 和 HTTP 服务器，不会发送真实请求。

推送到 `main`、`dev` 和 PR 时，GitHub Actions（`.github/workflows/ci.yml`）在 Linux 上检查 `gofmt -l .` 的输出为空，并运行 vet、测试和 Node.js 语法检查；race 检查和页面测试用于本地验证。只改动 Markdown、`docs/` 或 `assets/` 的提交不触发。同一分支的新提交会取消仍在运行的旧任务。

## 本地运行

```bash
go run . -listen 127.0.0.1:8317 -data-dir /tmp/lamplighter
```

然后打开 `http://127.0.0.1:8317/`。程序通过 `MAGPIE_ADDR` 找到 Magpie 网关，未设置时无法读取账号。

## 修改约定

- 读取 Magpie 的内部文件（如 `logins.json`）时，格式与预期不符要返回包装 `host.ErrFormat` 的错误，不猜测字段；对照 Magpie 最新源码中的 `internal/provider` 编写。
- 不刷新 Claude 和 Codex 的 token，原因见[与 Magpie 的接口](./architecture.md#与-magpie-的接口)。
- 修改额度请求的地址或请求头时，更新[架构文档](./architecture.md)中的表格。
- 修改轮询、跳过规则、点火时间或失败保护时，同步更新测试、README 和架构文档。
- 后台循环中的 goroutine 必须有 `recover`，取消监视器必须在调用结束前退出。HTTP 请求设置超时。
- 修改页面时保持 HTML、CSS、JS 分文件；文字插入 DOM 时使用 `textContent`。新增或修改页面文字时，同时修改 `page.js` 中 `I18N` 的中文和英文词条；新增或修改推送文字时，同时修改 `internal/notify/text.go` 的两列。增加一种语言时，两处各加一份。图表颜色只表示剩余额度的高低，使用与额度条相同的三档变量。
- 用户可见的行为、配置和限制写进 README（中英文同步），实现细节写进 `docs/`。

## 真实环境验证

点火会发送真实模型请求并开始新的 5 小时窗口。首次接入真实的 Magpie 时，先把 `ignition.enabled` 设为 `false`，确认额度、被动数据、页面和通知正常后再开启。接管点火前关闭 Magpie 自带的预热，以免重复点火。
