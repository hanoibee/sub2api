package middleware

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/requestarchive"
	"github.com/gin-gonic/gin"
)

func TestRequestArchiveMiddlewareScope(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tt := range []struct {
		name                string
		enabled, root       bool
		method, contentType string
		want                bool
	}{
		{"JSON inference", true, true, "POST", "application/json", true},
		{"disabled", false, true, "POST", "application/json", false},
		{"unconfigured", true, false, "POST", "application/json", false},
		{"websocket GET", true, true, "GET", "", false},
		{"multipart", true, true, "POST", "multipart/form-data; boundary=x", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.RequestArchive.Enabled = tt.enabled
			if tt.root {
				cfg.RequestArchive.Directory = t.TempDir()
				if err := requestarchive.Initialize(requestarchive.Config{Root: cfg.RequestArchive.Directory}, time.Now()); err != nil {
					t.Fatal(err)
				}
			}
			r := gin.New()
			r.Use(ClientRequestID(), RequestArchive(cfg))
			r.Handle(tt.method, "/test", func(c *gin.Context) {
				if got := requestarchive.FromContext(c.Request.Context()) != nil; got != tt.want {
					t.Errorf("scope=%v, want=%v", got, tt.want)
				}
				c.Status(200)
			})
			req := httptest.NewRequest(tt.method, "/test", nil)
			req.Header.Set("Content-Type", tt.contentType)
			r.ServeHTTP(httptest.NewRecorder(), req)
		})
	}
}
