# 大模型调用归档覆盖检查

检查日期：2026-10-14。代码基线：官方 v0.2.16 + 团队定制；仅检查面向客户端的模型调用，不计入认证、Token 刷新、模型发现、账户探测等辅助请求。

## 结论

尚未覆盖所有大模型调用。主流 HTTP JSON 文本推理已经接入归档，但是否最终生成目录还受入口 Content-Type、实际传输协议、上游路径、响应格式和完整性限制。不能用路由存在 RequestArchive 中间件来判断请求必然落盘。

这是源代码和现有自动化测试检查，不是生产流量全量对账；没有执行真实付费模型调用。

## 覆盖矩阵

| 模型调用路径 | 归档情况 | 依据/边界 |
| --- | --- | --- |
| Anthropic Messages、API Key 透传、OAuth HTTP | 已接入 | gateway_forward.go、gateway_anthropic_passthrough.go 调用 archiveHTTPUpstream |
| Chat Completions / Responses HTTP JSON 与 SSE | 已接入 | openai_plugin_transport.go 的 doOpenAIUpstream 同时包装核心 HTTP 和插件返回的 HTTP 响应 |
| Anthropic ↔ Chat Completions / Responses 协议转换 | 已接入 | gateway_forward_as_chat_completions.go、gateway_forward_as_responses.go 以及 openai_gateway_*_anthropic_native.go；保存实际发往上游的 JSON，不是转换前客户端正文 |
| Gemini API Key、OAuth、Vertex 原生与兼容接口 | 已接入 | gemini_messages_compat_service.go、gemini_chat_completions_compat_service.go；generateContent / streamGenerateContent 路径被 archiveProtocol 接受 |
| Antigravity HTTP 推理及其重试 | 已接入 | antigravity_gateway_upstream.go、antigravity_gateway_retry.go、antigravity_gateway_gemini.go |
| Bedrock 非流式 JSON | 已接入 | gateway_bedrock.go 中的 executeBedrockUpstream |
| Bedrock 二进制流式响应 | 未覆盖（首版明确排除） | application/vnd.amazon.eventstream 被 captureArchiveResponse 拒绝 |
| HTTP /responses 内部转 WebSocket 上游 | 未覆盖 | openai_gateway_forward.go 的 forwardOpenAIWSV2 分支绕过 HTTP 包装；客户端使用 POST 也不代表实际走 HTTP 上游 |
| 客户端 WebSocket Responses、Realtime、Live | 未覆盖（特殊传输） | 中间件排除 GET 和 WebSocket；Live 的 calls 路径及 SDP 响应也不在过滤器范围 |
| Embeddings | 未覆盖 | openai_embeddings.go 已走 doOpenAIUpstream，但 archiveProtocol 没有 /embeddings，响应不被包装 |
| TypeSafe / System One | 未覆盖 | gateway_systemone.go 直接调用 httpUpstream.Do，且 /systemone 不在 archiveProtocol 中 |
| Alpha Search 原生请求 | 未覆盖 | /alpha/search 不在 archiveProtocol 中；回退至 /responses 的调用可接入现有归档 |
| 原生图片、音频、视频生成 | 未覆盖 | 图片接口未被路径过滤器接受，Grok 媒体/音频直接调用 HTTPUpstream，multipart/二进制被首版排除 |
| Responses 内 image_generation 工具调用 | 条件覆盖 | 若实际走 HTTP /responses、上游 JSON/SSE 且正文没有 background=true，可以归档；WebSocket 分支不覆盖 |
| background Responses、独立异步/批量任务 | 未保证覆盖（首版排除） | captureArchiveResponse 明确跳过 background=true；不能据普通路由 Scope 推断后台任务有归档 |

同一 OpenAI 协议包装覆盖 DeepSeek、GLM、MiniMax 等兼容平台的条件是它们确实进入上述 Chat Completions / Responses / Messages HTTP 分支。平台名称本身不是覆盖保证。

## 重试与最终结果

Scope.Wrap 会结束并清理同一 Scope 中的上一尝试，再使用新请求和新响应替换它；正常重试和账户切换后，保留最后被包装且完整读取的尝试，不分别保存每次中间尝试。这符合现有归档需求。

若最后一次尝试没有上游响应、使用未支持的传输或被过滤器跳过，则不能宣称目录一定对应最终业务结果。尤其先前存在可归档响应、后续调用没有再次 Wrap 的混合路径需要额外回归验证。

## 统一落盘限制

- 必须启用 REQUEST_ARCHIVE_ENABLED 且配置 REQUEST_ARCHIVE_DIRECTORY。
- 客户端入口必须为 POST，Content-Type 包含 application/json，且不是 WebSocket Upgrade。缺少这个头的请求，即使正文是合法 JSON 且业务处理接受，也不会安装 Scope。
- 必须取得非空账户、可重放的 req.GetBody，以及被允许的实际上游 URL 路径。
- 上游响应 Content-Type 必须为空、JSON 或 text/event-stream；传输错误没有响应时不会开始归档。
- Scope.Finish 只有在响应完整且校验成功时才发布目录；不完整响应、客户端提前中断、写盘错误等情况可能清理临时目录，因此不能拿文件数量当作全部模型调用数量。

## 验证及后续优先级

合并后后端 go test ./... 通过，其中包含 requestarchive、service/request_archive 和中间件测试；前端固定依赖安装、i18n、TypeScript 和生产构建通过。现有测试明确验证模型发现、count_tokens、二进制事件流及 background 请求不产生归档，也验证 JSON/SSE 原始正文保存和已知协议映射。

建议优先处理普通 HTTP 客户端进入 WebSocket 上游的覆盖缺口；其次是 Embeddings、System One 和 Alpha Search。WebSocket、多媒体、异步任务需要单独定义归档单位和格式，不能简单加入 HTTP 路径名单。此轮仅检查并记录覆盖情况，没有扩大原首版归档范围。
