# 单次 API 请求与响应文件归档需求

状态：已按当前定稿实现；脱敏代码保留，但当前归档不启用脱敏，保存上游原始 JSON 数据。

## 1. 目标与范围

在 sub2api 中，为每一次面向客户端的大模型 API 调用建立独立目录，保存元数据、请求参数和 API 返回结果，以便按日期、请求 ID 检索和回溯。

归档单位是一次完整 API 请求及其响应，不是多轮聊天会话。一个 sessionId 可以关联多个请求目录；sessionId 缺失不影响归档。metadata.json 保存客户端提供的原始 sessionId。

本阶段先修改 sub2api，TokenHub 不在本阶段实现范围内。本文档阶段仅整理需求，不修改网关行为。

## 2. 已明确的需求

- 请求按天分目录，每次请求单独一个子目录。
- requestId 由归档模块独立生成，格式为 `req_yyyyMMdd_序号`，同一天内必须唯一。
- 每个请求目录最终包含且仅包含三个归档文件：
  1. `metadata.json`：元数据。
  2. `biz_request.json`：请求参数。
  3. 非流式为 `biz_response.json`；流式为 `biz_stream_response.jsonl`，两者不同时存在。
- 保存本次请求和响应的原始 JSON 数据，不截断、不重新序列化；当前不执行脱敏。
- 同时支持 STREAM 与 SYNC 两种请求模式。
- 收到上游响应时启动元数据、对应请求数据和响应数据的归档；流式响应在接收过程中持续追加。
- SSE 按事件顺序提取完整 data 载荷，每个事件的原始 JSON 保存为一行；不保留 SSE 前缀，不添加包装，不合并回答。
- 默认启用归档；归档根路径提供配置项，具体路径后续配置。
- 按固定 UTC+08:00（东八区）分日。
- providerCode 使用单一全局配置项。
- Token 元数据只区分总输入与总输出，不增加缓存、推理等明细字段。

## 3. 目录约定

```text
<archiveRoot>/
  2026-09-21/
    req_20260921_000001/
      metadata.json
      biz_request.json
      biz_response.json
    req_20260921_000002/
      metadata.json
      biz_request.json
      biz_stream_response.jsonl
  2026-09-22/
    req_20260922_000001/
      metadata.json
      biz_request.json
      biz_response.json
```

目录约定：

- archiveRoot 为可配置持久化目录，具体路径后续配置；归档默认启用。
- 日期按请求开始时间归属；跨午夜完成的请求仍放在开始日目录。
- 分日固定按 UTC+08:00（东八区），不受服务器本地时区影响。requestTime 使用 `yyyy-MM-dd HH:mm:ss` 格式。
- 归档 requestId 独立于 `usage_logs.request_id`，格式为 `req_yyyyMMdd_序号`，序号至少六位并在当天唯一，例如 `req_20260922_000001`。
- 服务启动时扫描当前日期的正式目录和临时占位目录，以两者最大序号初始化内存计数；初始化失败阻止服务启动。运行期间只在内存中同步递增，进入新日期时直接将序号重置为零，不再扫描目录。临时请求目录通过排他创建做一致性校验；若发生序号冲突，本次归档失败并记录失败指标，不递增重试、不覆盖已有目录。
- 分配过但最终未发布的编号允许形成空缺，不要求序号连续。
- 不直接使用未经校验的客户端标识或上游标识作为目录名。

## 4. 元数据 metadata.json

所有下列字段都必须存在。无法取得的值使用 JSON null，不以空字符串或 0 伪造已知值；实际测得的零 Token 使用 0。

| 字段 | 类型 | 含义与约定 |
| --- | --- | --- |
| requestId | string | 归档模块独立生成的请求标识，与请求目录名一致，格式为 `req_yyyyMMdd_序号`，同一天内必须唯一。 |
| sessionId | string / null | 客户端提供的会话标识；未提供时为 null。 |
| requestTime | string | 东八区请求开始时间，格式为 `yyyy-MM-dd HH:mm:ss`。 |
| httpStatus | integer / null | 本次归档所对应的上游 HTTP 响应状态码，不以网关转换后的客户端状态码代替。 |
| protocol | string / null | 本次归档对应的实际上游请求/响应协议；枚举按实际需要扩展。 |
| requestMode | string / null | STREAM 或 SYNC；描述本次归档的上游调用模式。 |
| modelName | string / null | 实际上游使用的模型名称，即路由和模型映射后本次调用实际请求的上游模型；发生重试或账户切换时，以最终服务方对应的上游模型为准。尚未确定上游模型时为 null，不用客户端请求的模型名代填。 |
| platform | string / null | 实际提供本次响应的上游平台；枚举见下文。尚未选定上游时为 null。 |
| inputTokens | integer / null | 本次调用的总输入 Token 数，不拆分缓存等明细；未取得时为 null。 |
| outputTokens | integer / null | 本次调用的总输出 Token 数，不拆分推理等明细；未取得时为 null。 |
| providerCode | string / null | 单一全局配置项指定的自定义编码，所有请求使用采集时的全局配置值，不随账户、渠道或上游切换而改变。不是密钥或上游生成的 ID；未配置时为 null。 |

### 4.1 协议枚举

```text
OPENAI
VERTEX
OPENAI_RESPONSES
ANTHROPIC
```

协议用于描述实际归档的上游请求/响应格式，与供应商平台分开。例如 ANTHROPIC 协议可以由 anthropic 或 anthropic_on_aws 平台承载。

字段名确定为 `protocol`。以上为初始协议枚举；新增协议按实际支持需要扩展，不强行映射成已有协议。

### 4.2 请求模式枚举

```text
STREAM
SYNC
```

### 4.3 平台枚举

```text
deepseek
qwen
hunyuan
glm
minimax
aws
anthropic_on_aws
anthropic
azure
openai
vertex
ai_studio
```

平台枚举为归档规范，不应直接假定与 sub2api 现有账户平台枚举一一对应。实施时需要建立显式映射。未列出的平台如何表示，应在实现前确定，不能误标为 openai。

### 4.4 示例

```json
{
  "requestId": "req_20260921_000001",
  "sessionId": "session-example-001",
  "requestTime": "2026-09-21 15:30:12",
  "httpStatus": 200,
  "protocol": "OPENAI",
  "requestMode": "STREAM",
  "modelName": "deepseek-chat",
  "platform": "deepseek",
  "inputTokens": 120,
  "outputTokens": 85,
  "providerCode": "deepseek-primary"
}
```

## 5. 采集时机与位置

已确定：在 sub2api 服务端收到上游响应时，再采集元数据、请求数据和响应数据。

本稿据此将归档对象统一为该响应对应的上游调用：

- biz_request.json 保存实际发给该上游的请求体，即模型映射、协议转换和请求改写后的数据。
- response 文件基于 sub2api 转换协议或改写响应之前收到的上游响应生成，当前直接保存原始 JSON 数据。
- metadata.json 的 modelName、platform、protocol、httpStatus 与这一上游调用保持一致；sessionId 来源于客户端会话标识，providerCode 来自全局配置。
- 请求发送时需要保留对应请求体和开始时间，供收到响应时使用；归档落盘在收到上游响应后启动，不把“开始保留请求上下文”混同于“开始归档”。
- 非流式响应读取完整正文后完成临时保存；流式随 SSE 到达持续写入临时文件，结束时补齐 Token 总量等最终元数据。三个文件全部成功后才发布正式请求目录。
- requestTime 仍为请求开始时间，而非上游响应到达时间；日期目录按 requestTime 的东八区日期归属。
- 上游返回 HTTP 错误也属于收到响应，首版范围内的 JSON 错误应保存。
- 尚未请求上游的本地拒绝，以及连接失败等完全未收到上游响应的场景，不触发本次规定的归档流程。
- 延续一次客户端 API 调用一个目录的约定。内部重试、账户切换不混合不同尝试的请求与响应；对应最终响应的上游调用写入该目录，中间尝试完整归档不在首版范围内。具体挂接点在实现时核实。
- HTTP 请求头、Authorization、Cookie 和账户凭据不属于 biz_request.json 的归档内容。

## 6. biz_request.json

保存与归档响应相对应、实际发送给上游的原始 JSON，包括该请求携带的历史消息、工具定义、模型参数及扩展字段。当前不执行脱敏，不能只存最后一条用户消息，也不能丢失未知字段。

对合法 JSON，保留实际发送的请求体，避免改动数值精度、字段或字符串内容。

非法 JSON、multipart、二进制上传和 WebSocket 多轮帧等特殊场景暂不纳入首版归档范围，保留为后续需求。不能把非法 JSON 或二进制内容伪装成有效 biz_request.json。

## 7. 非流式响应 biz_response.json

读取上游 API 返回的完整 JSON，按上游原始字节写入 biz_response.json，不重新序列化。正常 JSON 响应和 JSON 错误响应均按此规则保存，且不修改返回客户端的字节。

## 8. 流式响应 biz_stream_response.jsonl

每收到一个完整 SSE 数据事件，提取其原始 data JSON 追加到 biz_stream_response.jsonl，然后追加一个换行符。

- 写入的是事件原始 data JSON，不包含 SSE 的 data: 前缀、event: 等控制字段或事件分隔空行。
- 不重新序列化 JSON，不增加事件包装，不合并多个事件。
- 按上游事件顺序写入，一个事件一行。归档代码使用 SSE 读取逻辑取得完整事件，不能把任意网络读取块误当成完整事件。
- `[DONE]` 结束标志必须原样写入 biz_stream_response.jsonl，独占一行；不加引号，不转换成 JSON 字符串或对象。因此文件包含原始 JSON 数据行及结束标志行，不要求每一行都能按 JSON 解析。
- 元数据中的 Token 统计从原始内存数据解析；当前临时文件与正式归档均保存原始数据。
- 非 JSON 载荷、非单行 JSON 载荷和中断片段暂按特殊场景留待后续处理，不为了归档擅自转换格式；已经收到的完整 JSON 事件照常保存。

### 保存示例

从 SSE 数据事件取得原始 JSON，例如：

```json
{"id":"example-response","choices":[],"usage":{"prompt_tokens":120,"completion_tokens":85}}
```

将以上事件原样写入 biz_stream_response.jsonl，再追加换行。下一事件继续写下一行；收到 `[DONE]` 时仍原样写入一行。非流式完整 JSON 原样写入 biz_response.json。

## 9. 隐私与脱敏

脱敏实现保留在 `privacy.go`，但当前归档写入路径不调用脱敏函数。请求、响应及自由文本中的敏感内容会按原始数据保存，归档目录的访问控制和保存期限需要据此配置。

默认规则：

1. 对 password、secret、token、API key、Authorization、Cookie、私钥、credential、账户名、账户号、银行卡号、信用卡/借记卡、CVV/CVC、持卡人等敏感键递归替换为 `[REDACTED]`。
2. 对 email、phone、mobile、身份证号、地址、username、userId 等身份字段递归替换；规则同时覆盖常见中文字段名。
3. 对 thinking、reasoning_content、signature、encrypted_content 等推理或签名字段整体替换。
4. 对普通文本中的邮箱、电话号码/长数字标识、Bearer Token、JWT、常见 API Key 和带标签的密码/令牌片段进行模式脱敏。
5. data URI 图片、音频、视频、PDF，以及明显的长 Base64 媒体字段整体替换。
6. sessionId 按需求保留客户端提供的原值；缺失时为 null。
7. requestId、模型名、协议、平台、Token 数量和 HTTP 状态等非隐私业务字段保留。
8. `[DONE]` 不是 JSON，继续原样单独保存一行。
9. JSON 嵌套超过 64 层时停止递归，并原样保留超过限制的子树，避免因异常结构造成无限递归。

自动模式无法可靠识别人名、自由文本地址、病史等所有自然语言隐私。调用方仍应避免把不必要的敏感信息发送给模型；归档目录保持 0700、文件保持 0600，并应补充保存期限与访问控制。

## 10. 完整性、异常与写入生命周期

metadata.json 不保存 archiveStatus 和 responseComplete。响应完整性仍由归档实现内部判断，用于正确结束文件写入和处理异常，但不输出为业务元数据字段。

当前可保留 `error` 作为归档失败摘要，无错误时省略，且不得包含密钥。finishedTime、upstreamRequestId、gatewayRequestId、requestedModelName 等辅助字段暂不纳入本版。

写入生命周期：

1. 在请求上下文中保留开始时间和会话标识，此时不创建正式请求归档目录。
2. 收到对应上游响应时，由启动阶段已初始化的内存分配器生成独立 requestId，并在隐藏临时区排他创建占位目录。
3. 非流式响应在内存中收集完整 JSON 后原样写入；流式按事件顺序写入原始 data JSON，一事件一行。
4. 响应读取结束后补充总输入和总输出 Token。
5. 原子更新 metadata.json；确认 metadata.json、biz_request.json 和对应响应文件同时存在且响应完整后，将临时目录原子重命名为正式请求目录。
6. 任一文件缺失、响应中断、写盘失败或归档校验失败时，删除临时请求目录，不发布正式请求目录。

`.staging` 及其日期父目录是并发请求共享的基础目录，请求完成时不得删除；单次请求只能删除自己独占的 `req_*` 临时目录。空日期目录可由独立维护任务处理，不进入请求热路径。

保存期限、清理规则、容量控制及归档失败时是否阻断业务，本次均不定稿，后续再讨论。当前不引入自动删除策略。

流式响应按事件增量写入临时文件，非流式响应在内存中收集完整 JSON 后写入临时文件。当前两种模式均不脱敏；如后续重新启用脱敏或增加大小限制，需要重新核对临时文件安全策略。

## 11. 与 sub2api 现有功能的关系

- 现有 usage_logs 主要保存用量和计费信息，不能代替正文归档。
- 提示词审计默认关闭，按审核结果选择性保存提取文本，不能承担每次调用归档。
- 本功能应独立于提示词审计开关，开启归档后不因审核通过而跳过保存。
- 归档 requestId 独立生成，不复用 `usage_logs.request_id`，因此不会改变现有 ID 的生成、追踪及计费去重语义。
- WebSocket、异步任务及部分特殊计费事件可能使用上游 ID 或稳定任务 ID，不能由普通 HTTP 的 UUID 规则推断所有路径。特殊路径扩展时单独核实，本版无需为它们预先引入独立归档 ID。
- 现有 usage_logs.session_id 从客户端显式会话头提取。归档复用此来源；缺失时为 null。
- 普通 HTTP 路径上游返回的请求 ID 单独记录，不覆盖已复用的网关 requestId。

## 12. 验收标准

1. 一个正常非流式请求产生一个目录，包含 metadata.json、biz_request.json、biz_response.json。
2. 一个正常流式请求产生一个目录，包含 metadata.json、biz_request.json、biz_stream_response.jsonl。
3. 每日高并发、多实例情况下 `req_yyyyMMdd_序号` 不重复，任何已有归档不被覆盖。
4. 日期由开始时间和固定 UTC+08:00 决定，跨日请求不迁移目录。
5. 元数据必需字段齐全，枚举、类型与空值符合约定。
6. 未提供 sessionId 时仍可归档且 sessionId 为 null；提供时保存客户端原值。
7. 请求保留历史消息、工具参数、扩展字段；响应保留工具调用、usage 和错误内容。
8. 超过 65,536 字符的合法请求/响应不继承现有审计截断限制，按原始数据保存。
9. SSE 事件即使跨多次网络读取或一次读取包含多个事件，也能按事件顺序保存原始 JSON；一事件一行，无 SSE 前缀和自定义包装。
10. 客户端断开、流中断或磁盘写入失败时，不生成正式请求归档目录。
11. 上游重试或账户切换仍归属于一次客户端调用目录，modelName 和最终平台与最终服务方一致；providerCode 保持本次采集时的全局配置值。
12. 归档 requestId 独立于现有用量/追踪标识，现有计费去重行为不被改变。
13. 归档默认启用，保存路径由配置项提供，providerCode 来自单一全局配置。
14. 元数据使用 protocol 字段，协议枚举可扩展；Token 只记录总输入和总输出，不增加明细字段。

15. 收到上游响应后启动归档；请求与响应对应同一次上游调用，协议转换前的上游响应不被客户端出口版本替换。
16. 实际上游请求和客户端响应字节不因归档而改变；归档保存原始 JSON，不重新序列化，不丢失数值精度或原始空白。
17. 非流式 biz_response.json 为有效 JSON 文档，保留非敏感字段；敏感字段值替换为 `[REDACTED]`。
18. 上游发送 `[DONE]` 时，biz_stream_response.jsonl 必须保留一行原始 `[DONE]`，不加引号、不包装、不丢弃。

特殊场景不纳入首版验收。archiveStatus 和 responseComplete 明确不写入 metadata.json；其他辅助字段需另行确认。

## 13. 定稿记录与后续事项

### 13.1 已确定

1. 字段名称改为 protocol。
2. 默认启用归档；按固定 UTC+08:00 分日；保存路径提供配置项，实际值后续配置。
3. providerCode 为单一全局配置，不按账户或渠道分别配置。
4. 协议枚举按需扩展。
5. inputTokens / outputTokens 只保存总输入 / 总输出，不拆分明细。协议适配时应正确归一化供应商总量，避免重复累加已包含在总量中的缓存或推理 Token。
6. modelName 保存实际上游模型名称。
7. 收到上游响应时采集元数据、对应请求数据和响应数据；流式持续追加。
8. SSE 每个事件的原始 data JSON 保存为 biz_stream_response.jsonl 的一行，移除 SSE 前缀，不增加包装、不合并多个事件。
9. 非流式 biz_response.json 保存完整上游原始 JSON；流式每个原始事件写一行。
10. `[DONE]` 结束标志同样原样保存为一行。

### 13.2 仍为建议的辅助字段

archiveStatus、responseComplete 不写入 metadata.json；其他关联 ID 等辅助元数据未被本次确认自动纳入必需字段。实施时可单独讨论；不再把采集时机和 SSE 包装列为待确认项，且不得恢复 SSE 包装方案。

### 13.3 保留但暂不纳入首版

- WebSocket 多轮帧。
- 非 JSON 请求/响应、multipart、媒体二进制。
- 独立异步任务。

上述事项保留后续扩展入口，不要求首版覆盖。

### 13.4 本次暂不处理

- 保存期限和自动清理。
- 磁盘容量控制。
- 归档失败时是否阻断业务的策略。

后续再讨论，不作为本轮需求定稿的阻塞项；不擅自设置自动删除策略。

### 13.5 实施时需要补齐的配置与映射

- archiveRoot 的具体值后续配置；默认启用但未配置路径时的启动行为在实施时明确，不能无提示地写入任意路径。
- 现有 sub2api 平台与目标 platform 枚举的映射仍需按代码核实；协议按需扩展不等于允许错误标记平台。
