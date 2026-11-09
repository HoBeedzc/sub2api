package service

// =========================
// Admin webhook notification config
// =========================

// AdminWebhookNotifyEvents toggles individual admin webhook notification events.
type AdminWebhookNotifyEvents struct {
	OrderOfflinePending   bool `json:"order_offline_pending"`
	OrderRechargePaid     bool `json:"order_recharge_paid"`
	OrderSubscriptionPaid bool `json:"order_subscription_paid"`
	AccountAutoPaused     bool `json:"account_auto_paused"`
	APIKeyBreakerTripped  bool `json:"apikey_breaker_tripped"`
}

// AdminWebhookNotifyConfig is the persisted config for admin webhook notifications.
// Secret is write-only: it is stored in the JSON blob but never returned to clients
// (SecretConfigured is returned instead, mirroring the SMTP password pattern).
type AdminWebhookNotifyConfig struct {
	Enabled            bool                     `json:"enabled"`
	Provider           string                   `json:"provider"`
	WebhookURL         string                   `json:"webhook_url"`
	Secret             string                   `json:"secret,omitempty"`
	RateLimitPerMinute int                      `json:"rate_limit_per_minute"`
	AtUserIDs          []string                 `json:"at_user_ids"`
	Events             AdminWebhookNotifyEvents `json:"events"`
}

// AdminWebhookNotifyConfigUpdateRequest is the partial-update payload for the config.
type AdminWebhookNotifyConfigUpdateRequest struct {
	Enabled            *bool                     `json:"enabled"`
	WebhookURL         *string                   `json:"webhook_url"`
	Secret             *string                   `json:"secret"`
	ClearSecret        *bool                     `json:"clear_secret"`
	RateLimitPerMinute *int                      `json:"rate_limit_per_minute"`
	AtUserIDs          *[]string                 `json:"at_user_ids"`
	Events             *AdminWebhookNotifyEvents `json:"events"`
}

const (
	adminWebhookNotifyProviderFeishu = "feishu"

	adminWebhookNotifyRateLimitDefault = 50
	adminWebhookNotifyRateLimitMax     = 90
)

func defaultAdminWebhookNotifyConfig() *AdminWebhookNotifyConfig {
	return &AdminWebhookNotifyConfig{
		Enabled:            false,
		Provider:           adminWebhookNotifyProviderFeishu,
		WebhookURL:         "",
		Secret:             "",
		RateLimitPerMinute: adminWebhookNotifyRateLimitDefault,
		AtUserIDs:          []string{},
		Events: AdminWebhookNotifyEvents{
			OrderOfflinePending:   true,
			OrderRechargePaid:     true,
			OrderSubscriptionPaid: true,
			AccountAutoPaused:     true,
			APIKeyBreakerTripped:  true,
		},
	}
}

// normalizeAdminWebhookNotifyConfig fills invalid/empty fields with defaults.
func normalizeAdminWebhookNotifyConfig(cfg *AdminWebhookNotifyConfig) {
	if cfg == nil {
		return
	}
	// v1 only supports the Feishu custom bot; the field is kept for future channels.
	cfg.Provider = adminWebhookNotifyProviderFeishu
	if cfg.RateLimitPerMinute <= 0 {
		cfg.RateLimitPerMinute = adminWebhookNotifyRateLimitDefault
	}
	if cfg.RateLimitPerMinute > adminWebhookNotifyRateLimitMax {
		cfg.RateLimitPerMinute = adminWebhookNotifyRateLimitMax
	}
	if cfg.AtUserIDs == nil {
		cfg.AtUserIDs = []string{}
	}
}
