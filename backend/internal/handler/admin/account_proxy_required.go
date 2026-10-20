package admin

import (
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/gin-gonic/gin"
)

// [fork] A proxy is mandatory when an admin creates or edits a single account
// (POST/PUT /admin/accounts). Batch create, bulk edit and imports are not
// covered on purpose.

const accountProxyRequiredMessage = "proxy_id is required: every account must use a proxy"

// requireProxyForCreate rejects a create request without an existing proxy.
// It writes the error response and returns false when the request must stop.
func (h *AccountHandler) requireProxyForCreate(c *gin.Context, proxyID *int64) bool {
	if proxyID == nil || *proxyID <= 0 {
		response.BadRequest(c, accountProxyRequiredMessage)
		return false
	}
	return h.requireExistingProxy(c, *proxyID)
}

// requireProxyForUpdate rejects an edit that clears the proxy (the edit dialog
// sends 0 for "no proxy", so saving a proxyless account forces a choice).
// Requests that omit proxy_id, such as re-authorization, keep the current
// proxy and pass. Credential shadows always inherit the parent's proxy and the
// service ignores their proxy_id, so they are exempt.
func (h *AccountHandler) requireProxyForUpdate(c *gin.Context, accountID int64, proxyID *int64) bool {
	if proxyID == nil {
		return true
	}
	if *proxyID > 0 {
		return h.requireExistingProxy(c, *proxyID)
	}
	account, err := h.adminService.GetAccount(c.Request.Context(), accountID)
	if err != nil {
		response.ErrorFrom(c, err)
		return false
	}
	if account != nil && account.IsCredentialShadow() {
		return true
	}
	response.BadRequest(c, accountProxyRequiredMessage)
	return false
}

func (h *AccountHandler) requireExistingProxy(c *gin.Context, proxyID int64) bool {
	proxy, err := h.adminService.GetProxy(c.Request.Context(), proxyID)
	if err != nil || proxy == nil {
		response.BadRequest(c, "proxy_id does not refer to an existing proxy")
		return false
	}
	return true
}
