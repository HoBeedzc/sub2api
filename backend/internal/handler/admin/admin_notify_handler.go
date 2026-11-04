package admin

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

// AdminNotifyHandler exposes admin webhook notification config (Feishu bot).
type AdminNotifyHandler struct {
	adminNotifyService *service.AdminNotifyService
}

func NewAdminNotifyHandler(adminNotifyService *service.AdminNotifyService) *AdminNotifyHandler {
	return &AdminNotifyHandler{adminNotifyService: adminNotifyService}
}

// GetWebhookNotifyConfig returns the admin webhook notification config.
// GET /api/v1/admin/ops/webhook-notification/config
func (h *AdminNotifyHandler) GetWebhookNotifyConfig(c *gin.Context) {
	if h == nil || h.adminNotifyService == nil {
		response.Error(c, http.StatusServiceUnavailable, "Admin notify service not available")
		return
	}
	cfg, err := h.adminNotifyService.GetConfig(c.Request.Context())
	if err != nil {
		response.Error(c, http.StatusInternalServerError, "Failed to get webhook notification config")
		return
	}
	response.Success(c, cfg)
}

// UpdateWebhookNotifyConfig updates the admin webhook notification config.
// PUT /api/v1/admin/ops/webhook-notification/config
func (h *AdminNotifyHandler) UpdateWebhookNotifyConfig(c *gin.Context) {
	if h == nil || h.adminNotifyService == nil {
		response.Error(c, http.StatusServiceUnavailable, "Admin notify service not available")
		return
	}
	var req service.AdminWebhookNotifyConfigUpdateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request body")
		return
	}
	updated, err := h.adminNotifyService.UpdateConfig(c.Request.Context(), &req)
	if err != nil {
		var validationErr *service.AdminNotifyValidationError
		if errors.As(err, &validationErr) {
			response.Error(c, http.StatusBadRequest, validationErr.Error())
			return
		}
		// Storage/infra failures must not leak internal detail; log server-side only.
		slog.Error("admin_notify_config_update_failed", "err", err.Error())
		response.Error(c, http.StatusInternalServerError, "Failed to save webhook notification config")
		return
	}
	response.Success(c, updated)
}

// TestWebhookNotify sends a test card synchronously.
// POST /api/v1/admin/ops/webhook-notification/test
func (h *AdminNotifyHandler) TestWebhookNotify(c *gin.Context) {
	if h == nil || h.adminNotifyService == nil {
		response.Error(c, http.StatusServiceUnavailable, "Admin notify service not available")
		return
	}
	var req struct {
		WebhookURL         string   `json:"webhook_url"`
		Secret             string   `json:"secret"`
		AtUserIDs          []string `json:"at_user_ids"`
		IgnoreStoredSecret bool     `json:"ignore_stored_secret"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request body")
		return
	}
	if err := h.adminNotifyService.SendTest(c.Request.Context(), req.WebhookURL, req.Secret, req.AtUserIDs, req.IgnoreStoredSecret); err != nil {
		var validationErr *service.AdminNotifyValidationError
		if errors.As(err, &validationErr) {
			response.Error(c, http.StatusBadRequest, validationErr.Error())
			return
		}
		var internalErr *service.AdminNotifyInternalError
		if errors.As(err, &internalErr) {
			slog.Error("admin_notify_test_send_failed", "err", internalErr.Error())
			response.Error(c, http.StatusInternalServerError, "Failed to send test message")
			return
		}
		// Delivery failures (Feishu business errors, sanitized transport errors)
		// are admin-actionable; echo them on 400.
		response.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	response.Success(c, gin.H{"message": "Test message sent"})
}
