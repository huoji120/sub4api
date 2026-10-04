package admin

import (
	"net/http"

	"github.com/MACOS-DO/sub4api/internal/pkg/response"
	"github.com/gin-gonic/gin"
)

// SystemHandler handles system-related operations.
type SystemHandler struct {
	version string
}

// NewSystemHandler creates a handler with the locally injected build version.
func NewSystemHandler(version string) *SystemHandler {
	return &SystemHandler{version: version}
}

// GetVersion returns the local build version without contacting a release
// service. The value is injected from the server build information at startup.
// GET /api/v1/admin/system/version
func (h *SystemHandler) GetVersion(c *gin.Context) {
	response.Success(c, gin.H{"version": h.version})
}

// SelfUpdateDisabled rejects the retired self-update surface. It deliberately
// does not parse the request body, consult idempotency state, acquire a lock,
// contact an external service, mutate files, or restart the process.
func (h *SystemHandler) SelfUpdateDisabled(c *gin.Context) {
	response.ErrorWithDetails(
		c,
		http.StatusGone,
		"self-update is disabled",
		"SELF_UPDATE_DISABLED",
		nil,
	)
}
