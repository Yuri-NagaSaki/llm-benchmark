# 模型渠道基准测试方案

## 目标与层次

本方案验证工具测量的真实性、协议兼容性与可复现性。协议模拟用于证明解析及统计契约；本地 HTTP/TLS 集成用于证明真实传输行为；少量厂商冒烟用于证明当前账号和接口能够接通。三类证据不能互相替代。

不以远端 API 作为每次 CI 的依赖，不消耗真实模型额度，也不依赖真实密钥。Go 测试均使用确定的数据或 loopback `httptest`。TLS 测试将测试证书加入独立信任池，不使用 `InsecureSkipVerify`。

## 自动化覆盖矩阵

| 模块 | 必须覆盖的情况 | 测试入口 |
| --- | --- | --- |
| 配置 | 默认值、未知字段、冲突认证、HTTP 凭据、未知 profile、思考映射、非法超时、价格字段缺失与 null | `config_test.go`、`review_regression_test.go` |
| 请求构造 | Chat / Responses / Anthropic，根参数、受保护字段、多轮、图片、文件、工具历史、拒绝有损转换、现代 Chat token 上限字段 | `protocol_test.go` |
| SSE | LF / CRLF、分片、多行 data、注释、空帧、ID、BOM、EOF、1 MiB 大小上限、非法 JSON、冲突事件类型 | `sse_test.go`、`protocol_test.go` |
| 流状态 | 正文、reasoning-only、摘要、工具、completed / incomplete / failed、末尾 usage、显式 terminal、截断、错误状态不可恢复成成功 | `protocol_test.go`、`transport_test.go`、`review_regression_test.go` |
| 传输 | DNS/TCP/TLS 原生追踪，真实连接复用、新连接、超时、取消、429、request ID、响应体脱敏、单次请求无重试 | `transport_test.go` |
| 终止 | terminal 后连接保持开放，最多 25 ms 非计量清理，不能被连接拖尾改判模型超时；正文与协议结束分别计时 | `review_regression_test.go` |
| 调度 | 两渠道 Chat / Responses 同轮屏障、串行轮换、并发上限、唯一样本 ID、全部渠道归档 | `runner_test.go` |
| 预热 | 独立前缀位于系统内容之前，不修改原始工作负载；失败预热可追溯；null assistant 历史仍隔离 | `runner_test.go`、`review_regression_test.go` |
| 缓存 | cold candidate → hot → probe，两个 seed 方向，三渠道以上真实前驱、跨组前缀不同、128 可配置检查、部分命中 | `runner_test.go`、`metrics_test.go`、`review_regression_test.go` |
| 指标 | 推理与正文分离，单 token / 单块 null，缺 usage null，原始推理与摘要区分，非正文 token 不污染速率，n<100 禁止 p99 | `metrics_test.go`、`review_regression_test.go` |
| 成本 | 缓存读写、币种隔离、缺字段未知、失败请求已知成本仍统计、明确零价与缺失价格区分 | `metrics_test.go`、`review_regression_test.go` |
| 归档 | JSON 往返、汇总重放一致、0600 原子写入、错误保留、所有渠道、凭据在键和值中脱敏 | `report_test.go`、`main_test.go` |
| CLI | 混合协议比较、纯 JSON stdout、校验不需密钥、失败请求退出码 1 且归档、孤立 compare 参数退出码 2 | `cmd/llm-bench/main_test.go` |

## 执行命令

```bash
go test ./...
go test -race ./...
go vet ./...
go test -coverprofile=coverage.out ./...
go tool cover -func=coverage.out
```

每次实现变更后运行对应测试；最终统一执行 race、vet、格式检查、覆盖率、构建和配置校验。CI 使用最低声明版本 Go 1.26，发布包含 Linux amd64/arm64、macOS amd64/arm64 和 Windows amd64 的无 CGO 二进制。

## 厂商验收场景

进入正式渠道比较前，固定版本、请求内容、token 上限、思考模式、连接模式、运行区域及执行时间。一个矩阵中的不同 model ID 应先确认映射到预期模型；返回的 model 回显必须保留，不能因为名称相似就认定是同一版本。

1. **连通性与短输出**：默认思考模式下发短消息，验证 HTTP 状态、模型回显、request ID、完成原因和用量。只有单块时，首响应可用，解码速度不可测。
2. **关闭思考契约**：按渠道映射发关闭字段，要求明确 `reasoning_tokens == 0` 且没有推理或摘要事件。无法确认则失败并标 `unverifiable`。
3. **推理场景**：分别观察原始推理首响应、正文首响应、推理和正文窗口。用量耗尽导致 incomplete 属于有效的状态观测，不能改写成完成。
4. **稳态流输出**：使用能产生足够正文块的相同任务，固定输出上限。比较解码交付速度与完整请求速率，附有效样本数及失败率。
5. **网络模式**：独立测试 fresh 与 reuse。fresh 的 DNS/TCP/TLS 可能因本机 DNS 缓存、TLS 会话缓存和区域而变化；不通过相减推算纯模型排队。
6. **缓存**：使用满足厂商最小前缀长度的固定内容；先冷候选、后热、再跨协议探测。记录命中 token、未命中 token、比例、块对齐和顺序。三个以上 probe 可能受到前一个 probe 的缓存写入影响，不能把每一个都当作独立 pair。
7. **成本**：先填写已核验的币种、日期和单价，再根据实际 usage 估算。套餐、阶梯价、区域税费和账号折扣不在简单每百万 token 公式内，需要额外计费规则。

少量冒烟请求只证明当次调用行为；不能得出吞吐排名、长期缓存命中率、账号 TPM 或 p99。需要分析尾部时增加足够有效样本，并报告错误率与样本量，不把超时截断的分布当成全量延迟分布。

## 启动和本地测试性能

CLI 启动比较使用同一机器、相同 `--help` 场景和输出目的地。两个版本先各预热 3 次，随后交替顺序各启动 30 次；报告 p50、p95、最大值和二进制大小。该结果包含 CLI 解析与帮助渲染，不是模型 API 延迟。文件系统缓存不主动清空。

```bash
go run ./scripts/startup.go --new ./bin/llm-bench \
  --baseline /path/to/upstream-venv/bin/llm-bench --n 30 > startup.json
```

测试套件计时使用 `go test -count=1 ./...`，避免 Go 测试结果缓存；编译缓存仍存在。第一次编译、race 检测和冷缓存构建的时间应另列，不用缓存后的时间代表首次安装。
