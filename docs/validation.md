# 交付验证记录

验证日期：2026-10-12。Go 工具链 `go1.27.2 linux/amd64`；模块声明最低 Go 1.26。所有开发测试使用本地合成数据，真实厂商调用严格限制为 3 个短请求。

## 上游缺陷基线

固定上游 SHA 为 `4ed36ae9ee2c3d4b78450ce7c9b720f881d40f4e`。真实 OpenAI SDK 对象经上游 adapter → runner → stats 路径重放：reasoning-only 被判 `no content tokens received`；相同正文窗口加入 100 推理 token 后吞吐计算从 9 变成 109 tok/s；单个内容块计算为 0；真实比较 CLI 的 JSON 仅有 primary。6 个目标契约断言失败，已存在的 warmup 顺序控制通过。后者没有作为待修复缺陷重复宣称。

## Go 自动化验证

- 67 个顶层测试及其子场景通过。
- `go test -race ./... -count=1` 通过。
- `go vet ./...`、`gofmt` 和 `git diff --check` 通过。
- 全仓语句覆盖率 85.0%；核心 `internal/bench` 为 86.1%，CLI 为 77.1%。覆盖率不代表所有厂商行为都被覆盖。
- 两个示例配置均通过 `--validate`，校验不需要真实密钥。
- 本地 HTTPS 使用受信测试证书，实际验证新建连接、复用连接、握手事件和派生时间。
- 比较 CLI 实际运行本地 Chat 与 Responses，验证所有样本进入 JSON、纯 JSON stdout 和归档重放一致。

独立审查发现并修复 8 项具体问题：特殊消息的前缀隔离、缺失价格误为零、协议终止后等待 EOF、三渠道缓存前驱、非正文输出污染正文速度、JSON 键名泄露、孤立比较参数、自定义认证 Header 的 HTTPS 约束。修复引入的输入/输出模态边界回归也已修正，配对测试及原复现脚本均通过。

## 启动与本地测试速度

同机对比 `--help`，各预热 3 次，再交替运行各 30 次，输出丢弃，文件系统缓存保持自然状态。测量包含 CLI 解析和帮助渲染，不是 API 响应延迟。

| 实现 | 中位数 | p95 | 最大值 |
| --- | ---: | ---: | ---: |
| 上游 Python | 2240.29 ms | 2396.74 ms | 2531.40 ms |
| Go | 5.70 ms | 6.51 ms | 7.03 ms |

本机该场景的中位数约缩短 99.75%。Go 二进制为 7,356,679 bytes，约 7.0 MiB；不含第三方运行时依赖。Python 基线使用 CPython 3.12.13、OpenAI SDK 3.29.0、Anthropic SDK 1.13.0、Click 8.5.0、Rich 15.0.0。结果受机器、文件缓存、负载和依赖版本影响，不能推广成所有环境固定倍数。

可复现命令和全部 30 次样本保存在 [startup.json](evidence/startup.json)；文件也记录实际二进制 SHA-256、Go 源文件摘要，以及 `go test -count=1 ./...` 的 3 次命令耗时。测试结果缓存关闭，编译缓存已预热。

## 真实接口冒烟

唯一一轮发送 3 个并行短请求，预热 0、fresh 连接、最大输出 64 token。输入为 `Reply exactly OK.`。这些调用发生在最终 HTTP 终止清理修复前；该修复不影响以下首输出时间和用量。最终终止与连接清理行为由本地 HTTP/TLS 回归验证，没有追加厂商请求。

| 渠道 | HTTP | 结果 | 关键用量 | 首输出观察 |
| --- | --- | --- | --- | --- |
| DeepSeek 官方 Chat，默认思考 | 200 | incomplete / length | 输入 34，输出 64，推理 64，正文 0 | 首推理约 941 ms，正文首响应缺失 |
| 腾讯国际 Chat，关闭思考 | 200 | completed / stop，关闭检查 passed | 输入 8，输出 1，推理 0 | 正文约 1819 ms |
| 腾讯国际 Responses，关闭思考 | 200 | completed，关闭检查 passed | 输入 8，输出 1，推理 0 | 正文约 1855 ms |

DeepSeek 的结果证明 reasoning-only 和用量耗尽能够保真归档；不是“完整任务成功”。腾讯的单 token 回复没有解码窗口，因此 decode tok/s 为 null，完整请求输出速率可用。每渠道只有 1 次样本，不能作渠道速度排名或尾部统计。

公开脱敏摘要见 [live-smoke-summary.json](evidence/live-smoke-summary.json)。凭据未进入源码、配置、报告或提交；公开证据省略 request ID 和响应内容。腾讯国内、阿里云、DeepSeek Responses 和真实 Anthropic 未做当前账号调用，缓存共享与真实价格也未做厂商验收。

## 发布核验

初始源码提交 `7677ca44e720a185f9c046586a422b7dd9beb9cb` 的 [主分支 CI](https://github.com/Yuri-NagaSaki/llm-benchmark/actions/runs/38260100629) 和 [v1.0.0 发布工作流](https://github.com/Yuri-NagaSaki/llm-benchmark/actions/runs/38260312464) 均通过，远端实际使用 Go 1.26.8。

[v1.0.0](https://github.com/Yuri-NagaSaki/llm-benchmark/releases/tag/v1.0.0) 包含 Linux amd64/arm64、macOS amd64/arm64、Windows amd64 共 5 个二进制及 SHA256SUMS。已下载全部制品，校验和全部匹配；实际执行 Linux amd64 的 `--version`、`--validate` 成功。`go version -m` 确认源码提交为上述 SHA、`vcs.modified=false`、`CGO_ENABLED=0`。Linux 发布文件为 6,807,714 bytes，SHA-256 为 `386e45c98e0bd8e6facc02fec49168f72f5484245c797efa40514f352374cab3`。其他平台只做交叉编译和校验和验证，没有宣称在对应操作系统实跑。

首次 CI 提示旧 Actions 使用已弃用的 Node.js 20，随后将主分支工作流更新为官方当前 v7 标签，并固定 Ubuntu 24.04 runner。该维护提交不修改 Go 源码；v1.0.0 标签和已发布制品保持原始身份。
