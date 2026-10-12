# llm-benchmark

用单个 Go 可执行文件比较多个 LLM 渠道的流式响应、推理、缓存和成本。支持在同一轮中混合 Chat Completions、Responses 和 Anthropic Messages，归档每个请求的时间线、用量、错误与汇总。

这是对 [llm-bench](https://github.com/saksham10arora-dotcom/llm-bench) 测量契约的重新实现。运行时只使用 Go 标准库，无 Python、虚拟环境、Node.js 或第三方 Go 模块依赖。旧版 `llm-latency-bench` 的安装命令和 JSON 格式不适用于本仓库。

## 安装

使用 Go 1.26 或更新版本：

```bash
go install github.com/Yuri-NagaSaki/llm-benchmark/cmd/llm-bench@latest
llm-bench --version
```

从源码构建：

```bash
git clone https://github.com/Yuri-NagaSaki/llm-benchmark.git
cd llm-benchmark
go test ./...
CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o bin/llm-bench ./cmd/llm-bench
bin/llm-bench --help
```

推送代码会触发 CI 并生成 Linux 可执行制品。版本标签触发 Linux、macOS 和 Windows 构建及 SHA-256 校验文件；制品位于仓库的 Actions 或 Releases 页面。下载二进制后即可运行，不需要 Go 环境。

## 快速开始

密钥由环境变量或已有凭据管理器注入，只向命令传环境变量名。不要把密钥放进命令参数、JSON 配置或 Git 文件。

```bash
# 前提：DEEPSEEK_API_KEY 已由安全的本地方式设置。
llm-bench --base-url https://api.deepseek.com/v1 \
  --model deepseek-flash --api-key-env DEEPSEEK_API_KEY \
  --protocol chat --profile deepseek \
  --thinking default --prompt '用三句话解释 HTTP。' \
  --n 5 --max-tokens 256 --json results/deepseek.json

# 归档中的摘要可以离线重放。
llm-bench report results/deepseek.json
```

`--thinking off` 会根据 profile 和协议设置参数，并核对返回的推理信息。返回 `reasoning_tokens > 0`、原始推理或推理摘要时标失败；缺少明确的零推理用量时标记 `unverifiable`，也不会算验证通过。部分渠道不提供该字段，此时可用默认思考模式测量，但不能据此宣称思考已关闭。

## 多渠道与跨协议

```bash
# 仅校验配置和请求体，不读取密钥、不发送请求。
llm-bench --config examples/matrix.json --validate

# 所有已配置渠道都进入一份 JSON。
llm-bench --config examples/matrix.json --n 10 \
  --schedule parallel --json results/matrix.json

# 快速比较两个渠道，可分别设置协议、模型、key 环境变量及参数。
llm-bench --base-url https://api.deepseek.com/v1 \
  --model deepseek-flash --api-key-env DEEPSEEK_API_KEY --profile deepseek \
  --compare-base-url https://tokenhub-intl.tencentmaas.com/v1 \
  --compare-model deepseek-v4.1-flash --compare-api-key-env TENCENT_INTL_API_KEY \
  --compare-protocol responses --compare-profile tencent \
  --json results/cross-protocol.json
```

`examples/matrix.json` 是可编辑起点，不代表其中所有渠道已完成实测认证。不同区域 URL、模型可用性、限流、缓存块大小和收费标准应以账号当前配置为准。国内腾讯或阿里云渠道可添加新的 channel，设置自己的 `base_url`、`model`、`api_key_env` 和 profile；不要照搬其他区域的 URL。

并行模式同轮调度，在 `concurrency` 上限内执行；不能保证各厂商恰在同一时刻收到请求。JSON 保留轮次起点和各请求实际起点。串行模式按轮次轮换渠道顺序。默认每渠道每轮一个请求，账号压力测试需要另外固定并发场景；本工具不会把单流速度称为账号吞吐上限。

## 重要参数

| 参数 | 含义 |
| --- | --- |
| `--n` | 正式轮数，默认 5 |
| `--warmup` | 预热轮数，默认 0；独立前缀且保留样本 |
| `--connection reuse/fresh` | 每渠道复用连接，或每次请求使用新连接池 |
| `--timeout` | 整个请求的超时秒数，含读取流，默认 120 |
| `--connect-timeout` | 连接与 TLS 阶段超时秒数，默认 10 |
| `--thinking default/on/off` | 原样使用厂商默认行为，或通过明确映射打开/关闭思考 |
| `--effort` | 开启思考时的 effort；具体取值由渠道支持范围决定 |
| `--extra-body` | 根节点 JSON 对象或 `@file.json` |
| `--custom-headers` | Header 配置对象或 `@file.json`，值使用 `{ "env": "NAME" }` 或非秘密 `{ "value": "text" }` |
| `--messages` | 真正发送的多轮消息数组或 `@file.json` |
| `--mode cache` | 按固定序列执行独立前缀缓存实验 |
| `--json` | 归档路径，默认 `results/report.json`；`-` 输出纯 JSON |

配置文件与渠道 CLI 参数互斥，防止无意修改比较对象；运行选项可以覆盖配置文件。`extra_body` 可包含 `thinking`、`enable_thinking`、`reasoning`、工具、输出格式等厂商参数。模型、消息、流开关、token 上限和多候选数由工具管理，禁止用额外参数悄悄改变测量口径。显式思考模式的映射优先于额外参数；自定义渠道见 [custom-channel.json](examples/custom-channel.json)。

系统提示统一放在 `workload.messages` 的 system 消息中；`extra_body` 禁止覆盖 `system`、`instructions` 或注入隐藏的历史会话。仅接受文本与工具输出，音频输出和预测补全暂不支持。需要 `max_completion_tokens` 的现代 Chat 模型可在 channel 设置 `"token_limit_field": "max_completion_tokens"`；默认是 `max_tokens`，Responses 始终使用 `max_output_tokens`。

旧参数 `--api-key`、`--compare-api-key` 被环境变量名参数替代。`--task pdf/image/chat` 标签已移除；请通过消息内容发送真实图片、文件或多轮对话。Chat 到 Responses 支持文本、图片、文件和工具消息转换；不支持或会丢失含义的转换在请求前报错。示例多轮消息见 [messages.json](examples/messages.json)。

## 指标如何解读

| 指标 | 定义与限制 |
| --- | --- |
| 正文 / 推理首响应 | 请求起点到相应首个非空 delta，分别记录；推理摘要有独立指标 |
| DNS / TCP / TLS | `httptrace` 观测阶段；复用连接时本次握手为缺失，JSON 保留连接尝试事件 |
| 连接后到首事件 | 包含网络、网关、排队与预填充，**不是纯服务端排队时间** |
| 正文解码 tok/s | 已知正文与首块 token 数时，按首末正文块窗口计算；仍是流交付速度 |
| 正文近似 tok/s | 首块 token 数未知时才使用 `(正文 token - 1) / 正文窗口`，名称明确标估算 |
| 推理解码 tok/s | 只使用原始推理流及对应 token 数；绝不用摘要流替代 |
| 输出全程 tok/s | 所有报告的输出 token（含推理）/ 从请求起点到协议终止的耗时；适合短输出辅助比较 |
| 单路 tok/min | 输出全程 tok/s × 60；不等于账号 TPM |
| 账号 TPM | 仅读取显式配置的配额响应头；没有则为 null |
| 缓存 | 输入、命中、未命中、写入 token，命中比例、部分命中和可选块对齐检查 |
| 成本 | 按渠道显式价格、币种、来源日期和实际计费 token 估算；缺失信息为 null |

SSE chunk 不是 token，分块间隔不能解释为逐 token 延迟。只收到一个块时，解码速度为 null，首响应和总时间仍可用。没有 usage 时不按单词或字符数猜 token。正文 token 仅从显式正文用量，或无工具输出且已知 `output_tokens - reasoning_tokens` 推导。

少于 100 个有效样本时尾部显示「最慢一次 / max」。p99 仅在有效样本数达到 100 后报告，依然是经验分位数。失败与不完整样本保留在状态计数里，性能分位数只使用完成样本。比较失败率时必须一起看完成数，不能仅凭幸存样本的低延迟选渠道。

## 缓存实验与定价

```bash
llm-bench --config examples/matrix.json --mode cache --n 1 \
  --warmup 1 --json results/cache.json
```

缓存模式忽略普通并发调度，按渠道顺序串行执行。每个渠道轮流成为 seed：为完整输入开头加入唯一前缀，先请求一次冷候选，再请求一次热样本，随后让其他渠道依次探测相同语义前缀。两个渠道每轮 6 个请求，四个渠道每轮 20 个请求；另加预热。JSON 记录 `cache_group` 和 `cache_predecessor`。

三个以上渠道时，后面的 probe 可能受到前一个 probe 写入缓存的影响，JSON 的前驱链保留实际执行顺序。需要独立两两归因时，每次仅配置两个渠道。

应使用足够长且固定的输入验证缓存。短提示可能达不到厂商最小缓存长度。冷候选并不保证零命中，跨协议探测命中也不单独证明共享缓存；隐藏前缀和系统包装会影响结果。`cache_block_size` 可设为 128，但只对已确认采用此规则的渠道启用。

不内置无法及时维护的厂商价格常量。可在 channel 中添加 `pricing`，单位均为每百万 token；以下是结构示例，数字仅演示计算，不能用于真实成本报告：

```json
{
  "currency": "USD",
  "input_per_million": 1.0,
  "output_per_million": 2.0,
  "cached_input_per_million": 0.1,
  "source": "example only; replace with verified pricing URL",
  "as_of": "2026-10-12"
}
```

缓存写入收费可用 `cache_write_per_million` 单独配置。成本按币种分别汇总，缺失成本样本不会当作免费请求；输出标明已知成本样本数。

## 归档与运维

报告使用 `schema_version=1.0`，包括配置、环境、起止时间、全部样本和摘要。每个样本保留 UTC 请求时间、单调时钟毫秒偏移、原始 usage 对象、模型回显、request ID、HTTP 状态和结束原因。默认不保存正文或推理文本，只保存事件类型、字符数和时间；输入配置会归档，因此不要使用含敏感业务内容的测试输入。

失败响应最多保留 4 KiB 摘要并脱敏。凭据只从指定环境变量读取；认证 Header 不进入归档。报告以 0600 权限原子写入。默认不跟随重定向、不使用系统代理、不自动重试，避免凭据转发和隐含的测量变化。Ctrl-C 会取消正在执行的请求并保存已调度样本。退出码：0 完成；1 正式样本存在失败或不完整；2 配置或归档失败；130 中断。

收到协议终止事件后立即结束模型计量，最多额外等待 25 ms 读取 HTTP 尾部，以便正常连接继续复用。连接清理时间单独记录为 `connection_cleanup`；网关保持流不关闭时，不把拖尾改判为模型超时。完整 HTTP EOF 如可观察，记录为 `transport_eof`。请求后端异常流或缺少结束原因仍会失败。

需求和限制见 [requirements.md](docs/requirements.md)，测试方案见 [test-plan.md](docs/test-plan.md)，协议来源见 [sources.md](docs/sources.md)，交付验证见 [validation.md](docs/validation.md)。
