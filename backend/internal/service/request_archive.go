package service

import (
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/requestarchive"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/tidwall/gjson"
)

// archiveHTTPUpstream 只包装推理调用，不包装 Token 刷新、账户探测或模型发现。
// 请求 Scope 由网关路由安装。
func archiveHTTPUpstream(upstream HTTPUpstream, account *Account) HTTPUpstream {
	return &archivingUpstream{HTTPUpstream: upstream, account: account}
}

type archivingUpstream struct {
	HTTPUpstream
	account *Account
}

func (u *archivingUpstream) Do(req *http.Request, proxy string, id int64, concurrency int) (*http.Response, error) {
	resp, err := u.HTTPUpstream.Do(req, proxy, id, concurrency)
	if err == nil {
		captureArchiveResponse(req, resp, u.account)
	}
	return resp, err
}
func (u *archivingUpstream) DoWithTLS(req *http.Request, proxy string, id int64, concurrency int, profile *tlsfingerprint.Profile) (*http.Response, error) {
	resp, err := u.HTTPUpstream.DoWithTLS(req, proxy, id, concurrency, profile)
	if err == nil {
		captureArchiveResponse(req, resp, u.account)
	}
	return resp, err
}
func captureArchiveResponse(req *http.Request, resp *http.Response, account *Account) {
	if req == nil || req.URL == nil || resp == nil || resp.Body == nil || account == nil {
		return
	}
	scope := requestarchive.FromContext(req.Context())
	if scope == nil || req.Method != http.MethodPost {
		return
	}
	protocol := archiveProtocol(req.URL.Path)
	if protocol == "" {
		return
	}
	contentType := strings.ToLower(resp.Header.Get("Content-Type"))
	// Bedrock 二进制事件流、媒体和非 JSON 响应不属于首版范围。
	// Gemini 原生 JSON 和 SSE 继续使用下方常规路径。
	if contentType != "" && !strings.Contains(contentType, "json") && !strings.Contains(contentType, "text/event-stream") {
		return
	}
	if req.GetBody == nil {
		slog.Warn("request archive skipped: upstream request is not replayable", "account_id", account.ID)
		return
	}
	body, err := req.GetBody()
	if err != nil {
		slog.Error("request archive snapshot failed", "error", err)
		return
	}
	raw, err := io.ReadAll(body)
	_ = body.Close()
	if err != nil {
		slog.Error("request archive snapshot failed", "error", err)
		return
	}
	if !gjson.ValidBytes(raw) || gjson.GetBytes(raw, "background").Bool() {
		return
	}
	model := gjson.GetBytes(raw, "model").String()
	if model == "" {
		model = archiveURLModel(req.URL)
	}
	stream := gjson.GetBytes(raw, "stream").Bool() || strings.Contains(strings.ToLower(req.URL.Path), "stream")
	eventStream := strings.Contains(contentType, "text/event-stream")
	if eventStream {
		stream = true
	}
	if contentType == "" && stream && resp.StatusCode < 400 {
		eventStream = true
	}
	info := requestarchive.Info{Protocol: protocol, Model: model, Platform: archivePlatform(account, req.URL), Stream: stream, Status: resp.StatusCode}
	resp.Body = scope.Wrap(resp.Body, raw, info, eventStream, resp.ContentLength)
}
func archiveProtocol(path string) string {
	p := strings.ToLower(path)
	switch {
	case strings.HasSuffix(p, "/messages"), strings.Contains(p, ":rawpredict"), strings.Contains(p, ":streamrawpredict"), strings.HasSuffix(p, "/invoke"), strings.HasSuffix(p, "/invoke-with-response-stream"):
		return "ANTHROPIC"
	case strings.HasSuffix(p, "/chat/completions"), strings.HasSuffix(p, "/completions"):
		return "OPENAI"
	case strings.HasSuffix(p, "/responses"), strings.HasSuffix(p, "/responses/compact"):
		return "OPENAI_RESPONSES"
	case strings.Contains(p, "generatecontent"):
		return "VERTEX"
	default:
		return ""
	}
}
func archiveURLModel(u *url.URL) string {
	path := u.Path
	if start := strings.LastIndex(path, "/models/"); start >= 0 {
		model := path[start+8:]
		if end := strings.LastIndex(model, ":"); end >= 0 {
			model = model[:end]
		}
		return model
	}
	if start := strings.Index(path, "/model/"); start >= 0 {
		model := path[start+7:]
		if end := strings.LastIndex(model, "/"); end >= 0 {
			model = model[:end]
		}
		return model
	}
	if start := strings.Index(path, "/deployments/"); start >= 0 {
		model := path[start+13:]
		if end := strings.Index(model, "/"); end >= 0 {
			model = model[:end]
		}
		return model
	}
	return ""
}
func archivePlatform(account *Account, u *url.URL) string {
	host := strings.ToLower(u.Hostname())
	switch {
	case account.IsBedrock():
		return "anthropic_on_aws"
	case account.IsVertexServiceAccount(), strings.Contains(host, "aiplatform.googleapis.com"):
		return "vertex"
	case strings.HasSuffix(host, ".openai.azure.com"), strings.HasSuffix(host, ".services.ai.azure.com"):
		return "azure"
	case strings.Contains(host, "dashscope"):
		return "qwen"
	case strings.Contains(host, "hunyuan"):
		return "hunyuan"
	case strings.Contains(host, "deepseek"):
		return "deepseek"
	case strings.Contains(host, "bigmodel"):
		return "glm"
	case strings.Contains(host, "minimax"):
		return "minimax"
	case account.Platform == PlatformZhipu:
		return "glm"
	case account.Platform == PlatformGemini && account.Type == AccountTypeAPIKey:
		return "ai_studio"
	case account.Platform == PlatformGemini:
		return "vertex"
	default:
		// 保留未支持的平台名称，避免错误标记为 OpenAI。
		return account.Platform
	}
}
