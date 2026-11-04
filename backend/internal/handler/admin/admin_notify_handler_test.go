//go:build unit

package admin

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// handlerNotifySettingRepo stubs the settings storage for handler tests. Only
// the methods the admin-notify service touches are implemented; the embedded
// interface stays nil and would panic on anything unexpected (test failure).
type handlerNotifySettingRepo struct {
	service.SettingRepository
	data   map[string]string
	setErr error
}

func (m *handlerNotifySettingRepo) GetValue(_ context.Context, key string) (string, error) {
	v, ok := m.data[key]
	if !ok {
		return "", service.ErrSettingNotFound
	}
	return v, nil
}

func (m *handlerNotifySettingRepo) Set(_ context.Context, key, value string) error {
	if m.setErr != nil {
		return m.setErr
	}
	m.data[key] = value
	return nil
}

func newNotifyTestContext(t *testing.T, method, path, body string) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	c.Request = req
	return c, rec
}

func TestAdminNotifyHandlerUpdateConfig_ValidationErrorIs400(t *testing.T) {
	repo := &handlerNotifySettingRepo{data: map[string]string{}}
	h := NewAdminNotifyHandler(service.NewAdminNotifyService(repo))

	c, rec := newNotifyTestContext(t, http.MethodPut, "/admin/ops/webhook-notification/config", `{"enabled": true}`)
	h.UpdateWebhookNotifyConfig(c)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "webhook url is required")
}

func TestAdminNotifyHandlerUpdateConfig_StorageFailureIs500AndSanitized(t *testing.T) {
	repo := &handlerNotifySettingRepo{data: map[string]string{}, setErr: errors.New("db down: connection refused")}
	h := NewAdminNotifyHandler(service.NewAdminNotifyService(repo))

	c, rec := newNotifyTestContext(t, http.MethodPut, "/admin/ops/webhook-notification/config", `{"webhook_url": "https://open.feishu.cn/hook/x"}`)
	h.UpdateWebhookNotifyConfig(c)

	require.Equal(t, http.StatusInternalServerError, rec.Code)
	require.NotContains(t, rec.Body.String(), "db down", "internal error detail must not be echoed")
	require.NotContains(t, rec.Body.String(), "connection refused")
}

func TestAdminNotifyHandlerTestWebhook_NotConfiguredIs400(t *testing.T) {
	repo := &handlerNotifySettingRepo{data: map[string]string{}}
	h := NewAdminNotifyHandler(service.NewAdminNotifyService(repo))

	c, rec := newNotifyTestContext(t, http.MethodPost, "/admin/ops/webhook-notification/test", `{}`)
	h.TestWebhookNotify(c)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "webhook url is not configured")
}
