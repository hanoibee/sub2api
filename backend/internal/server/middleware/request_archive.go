package middleware

import (
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/requestarchive"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// RequestArchive 只为客户端 JSON 推理请求启用归档。中间件本身不读取请求体，
// 也不创建目录；收到上游响应后才开始采集。未配置目录时应明确提示，
// 不得把请求数据写入任意工作目录。
func RequestArchive(cfg *config.Config) gin.HandlerFunc {
	if cfg == nil || !cfg.RequestArchive.Enabled {
		return func(c *gin.Context) { c.Next() }
	}
	if strings.TrimSpace(cfg.RequestArchive.Directory) == "" {
		slog.Warn("request archive enabled but directory is unset; set REQUEST_ARCHIVE_DIRECTORY to start writing archives")
		return func(c *gin.Context) { c.Next() }
	}
	archiveConfig := requestarchive.Config{Root: cfg.RequestArchive.Directory, ProviderCode: cfg.RequestArchive.ProviderCode}
	return func(c *gin.Context) {
		if c.Request == nil || c.Request.Method != http.MethodPost || !strings.Contains(strings.ToLower(c.GetHeader("Content-Type")), "application/json") || strings.EqualFold(c.GetHeader("Upgrade"), "websocket") {
			c.Next()
			return
		}
		ctx, scope := requestarchive.WithScope(c.Request.Context(), archiveConfig, service.ExtractClientSessionID(c), time.Now())
		c.Request = c.Request.WithContext(ctx)
		defer scope.Finish()
		c.Next()
	}
}
