package admin

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// missingProxyAdminService reports every proxy as nonexistent.
type missingProxyAdminService struct {
	*stubAdminService
}

func (s *missingProxyAdminService) GetProxy(context.Context, int64) (*service.Proxy, error) {
	return nil, errors.New("proxy not found")
}

func serveAccountProxyRequest(t *testing.T, adminSvc service.AdminService, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	handler := NewAccountHandler(adminSvc, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
	router.POST("/accounts", handler.Create)
	router.PUT("/accounts/:id", handler.Update)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)
	return rec
}

func TestAccountCreateRequiresProxy(t *testing.T) {
	const base = `{"name":"account","platform":"anthropic","type":"apikey","credentials":{"api_key":"x"}`
	cases := []struct {
		name string
		body string
		want int
	}{
		{"missing", base + `}`, http.StatusBadRequest},
		{"null", base + `,"proxy_id":null}`, http.StatusBadRequest},
		{"zero", base + `,"proxy_id":0}`, http.StatusBadRequest},
		{"present", base + `,"proxy_id":5}`, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := newStubAdminService()
			rec := serveAccountProxyRequest(t, svc, http.MethodPost, "/accounts", tc.body)
			require.Equal(t, tc.want, rec.Code, rec.Body.String())
			if tc.want == http.StatusOK {
				require.Len(t, svc.createdAccounts, 1)
			} else {
				require.Empty(t, svc.createdAccounts)
				require.Contains(t, rec.Body.String(), "proxy_id is required")
			}
		})
	}
}

func TestAccountCreateRejectsNonexistentProxy(t *testing.T) {
	stub := newStubAdminService()
	rec := serveAccountProxyRequest(t, &missingProxyAdminService{stub}, http.MethodPost, "/accounts",
		`{"name":"account","platform":"anthropic","type":"apikey","credentials":{"api_key":"x"},"proxy_id":5}`)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "existing proxy")
	require.Empty(t, stub.createdAccounts)
}

func TestAccountUpdateRequiresProxy(t *testing.T) {
	t.Run("clearing_proxy_rejected", func(t *testing.T) {
		svc := newStubAdminService()
		rec := serveAccountProxyRequest(t, svc, http.MethodPut, "/accounts/3", `{"name":"account","proxy_id":0}`)
		require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		require.Nil(t, svc.lastUpdateAccountInput)
	})

	t.Run("setting_proxy_allowed", func(t *testing.T) {
		svc := newStubAdminService()
		rec := serveAccountProxyRequest(t, svc, http.MethodPut, "/accounts/3", `{"name":"account","proxy_id":5}`)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.NotNil(t, svc.lastUpdateAccountInput)
	})

	t.Run("nonexistent_proxy_rejected", func(t *testing.T) {
		svc := newStubAdminService()
		rec := serveAccountProxyRequest(t, &missingProxyAdminService{svc}, http.MethodPut, "/accounts/3", `{"proxy_id":5}`)
		require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		require.Nil(t, svc.lastUpdateAccountInput)
	})

	t.Run("omitted_proxy_passes_for_reauth", func(t *testing.T) {
		svc := newStubAdminService()
		rec := serveAccountProxyRequest(t, svc, http.MethodPut, "/accounts/3", `{"type":"oauth","credentials":{"access_token":"t"}}`)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.NotNil(t, svc.lastUpdateAccountInput)
	})

	t.Run("credential_shadow_exempt", func(t *testing.T) {
		svc := newStubAdminService()
		parentID := int64(1)
		svc.getAccountResult = &service.Account{ID: 3, Name: "shadow", Status: service.StatusActive, ParentAccountID: &parentID}
		rec := serveAccountProxyRequest(t, svc, http.MethodPut, "/accounts/3", `{"name":"shadow","proxy_id":0}`)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.NotNil(t, svc.lastUpdateAccountInput)
	})
}
