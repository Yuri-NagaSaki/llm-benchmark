# 协议与测量来源

核验日期：2026-10-12。接口可能随厂商演进；profile 是请求参数映射，不能代替真实账号的功能验收。

| 来源 | 用途 |
| --- | --- |
| [Go httptrace.ClientTrace](https://pkg.go.dev/net/http/httptrace) | DNS、Connect、TLS、GotConn、WroteRequest、首响应字节的原生观测；回调可能并发执行 |
| [Go http.Client](https://pkg.go.dev/net/http#Client) | 连接复用、取消、超时和重定向语义 |
| [OpenAI Responses streaming events](https://developers.openai.com/api/reference/resources/responses/streaming-events) | output_text、reasoning_text、reasoning_summary、completed、incomplete、failed 事件与用量字段 |
| [OpenAI Responses create](https://developers.openai.com/api/reference/resources/responses/methods/create) | 输入项、工具与多模态字段、输出 token 和推理 token 的关系 |
| [DeepSeek Chat](https://api-docs.deepseek.com/api/create-chat-completion) | thinking.type enabled/disabled、reasoning_content、缓存及 completion 用量 |
| [DeepSeek Responses](https://api-docs.deepseek.com/api/create-response) | reasoning.effort none/low/high/max；不能把 Chat 的 thinking 字段当作 Responses 的开关 |
| [DeepSeek thinking guide](https://api-docs.deepseek.com/guides/thinking_mode) | 思考模式和推理内容的处理 |
| [上游固定基线](https://github.com/saksham10arora-dotcom/llm-bench/tree/4ed36ae9ee2c3d4b78450ce7c9b720f881d40f4e) | 原有测量、导出和 CLI 的故障复现 |

文档检索使用 Context7 MCP：Go 标准库索引为 Go 1.25.3，所用 `net/http`、`httptrace` 和测试接口同时以本地 Go 1.27.2 编译、测试验证。模块最低版本定为 Go 1.26，CI 另行验证这一版本。Responses 官方页面已实际读取。腾讯与阿里云 profile 基于用户提供的既有实测约定；只把单独记录的冒烟结果算作当前验证，不对未调用的区域或接口作能力承诺。

思考摘要可能是压缩后或延迟生成的说明，并不等于完整推理流。可用量不代表每一个 token 都有独立 SSE 时间戳。因此指标区分原始推理、推理摘要、解码近似和完整请求速率。
