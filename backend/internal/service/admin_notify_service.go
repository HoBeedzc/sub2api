package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// =========================
// Admin webhook notifications (Feishu custom bot)
// =========================

// Admin notify event types; each maps to a toggle in AdminWebhookNotifyEvents.
const (
	AdminNotifyEventOrderRechargePaid     = "order_recharge_paid"
	AdminNotifyEventOrderSubscriptionPaid = "order_subscription_paid"
	AdminNotifyEventAccountAutoPaused     = "account_auto_paused"
	AdminNotifyEventAPIKeyBreakerTripped  = "apikey_breaker_tripped"
	AdminNotifyEventTest                  = "test"
)

// Admin notify severities; they map to Feishu card header colors.
const (
	AdminNotifySeverityInfo     = "info"
	AdminNotifySeverityWarning  = "warning"
	AdminNotifySeverityCritical = "critical"
)

const (
	adminNotifyQueueCapacity = 100

	// adminNotifyConfigTTLC is the worker-side config cache TTL. The cache and
	// InvalidateConfig are per-process: with multiple gateway replicas, a config
	// saved on one instance is picked up by the others only after their own TTL
	// expires (≤30s), so cross-instance changes converge with that delay.
	adminNotifyConfigTTLC = 30 * time.Second

	// adminNotifySendTimeout bounds one full delivery: up to 3 attempts at
	// adminNotifyFeishuHTTPTimeout each, plus both retry backoffs and slack.
	// With a shorter budget the outer context would cut the chain short and
	// the third attempt would never fire.
	adminNotifySendTimeout = 3*adminNotifyFeishuHTTPTimeout + adminNotifyRetryTotalBackoff

	// Quota auto-pause is a scheduling veto evaluated per candidate per request
	// and never persisted, so identical pauses repeat on every request. Cool the
	// notification down per account to keep the feed readable.
	adminNotifyQuotaPauseCooldown = 30 * time.Minute
)

// AdminNotifyField is one key-value line rendered in the notification card.
type AdminNotifyField struct {
	Label string
	Value string
}

// AdminNotifyEvent is a single admin notification occurrence.
type AdminNotifyEvent struct {
	Type     string // one of the AdminNotifyEvent* constants
	Severity string // one of the AdminNotifySeverity* constants
	Title    string
	Fields   []AdminNotifyField
}

// AdminWebhookNotifyConfigView is the client-facing config with the secret redacted.
type AdminWebhookNotifyConfigView struct {
	Enabled            bool                     `json:"enabled"`
	Provider           string                   `json:"provider"`
	WebhookURL         string                   `json:"webhook_url"`
	SecretConfigured   bool                     `json:"secret_configured"`
	RateLimitPerMinute int                      `json:"rate_limit_per_minute"`
	AtUserIDs          []string                 `json:"at_user_ids"`
	Events             AdminWebhookNotifyEvents `json:"events"`
}

// AdminNotifyService queues admin events and delivers them to the configured
// webhook. Producers only enqueue (non-blocking); the single worker goroutine
// owns config loading, rate limiting and HTTP delivery, so request hot paths
// never wait on network or settings storage.
type AdminNotifyService struct {
	settingRepo SettingRepository
	httpClient  *adminNotifyHTTPClient

	queue   chan AdminNotifyEvent
	limiter *slidingWindowLimiter

	stopCh   chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup

	cooldownMu     sync.Mutex
	cooldownSentAt map[string]time.Time // key -> last NotifyCooldown emission

	cfgMu     sync.Mutex
	cachedCfg *AdminWebhookNotifyConfig
	cachedAt  time.Time
}

func NewAdminNotifyService(settingRepo SettingRepository) *AdminNotifyService {
	return &AdminNotifyService{
		settingRepo:    settingRepo,
		httpClient:     newAdminNotifyHTTPClient(adminNotifyFeishuHTTPTimeout),
		queue:          make(chan AdminNotifyEvent, adminNotifyQueueCapacity),
		limiter:        newSlidingWindowLimiter(0, time.Minute),
		stopCh:         make(chan struct{}),
		cooldownSentAt: make(map[string]time.Time),
	}
}

// Start launches the delivery worker and registers the hot-path hook.
func (s *AdminNotifyService) Start() {
	if s == nil {
		return
	}
	s.wg.Add(1)
	go s.run()
	adminNotifyHotPath.Store(s)
}

// Stop terminates the worker. Queued-but-undelivered events are dropped.
func (s *AdminNotifyService) Stop() {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() { close(s.stopCh) })
	s.wg.Wait()
	if cur := adminNotifyHotPath.Load(); cur == s {
		adminNotifyHotPath.Store(nil)
	}
}

func (s *AdminNotifyService) run() {
	defer s.wg.Done()
	// The delivery context is cancelled as soon as stopCh closes so an
	// in-flight delivery (HTTP attempt or backoff wait) aborts promptly;
	// otherwise Stop() would block for the full ~21s retry budget.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-s.stopCh:
			cancel()
		case <-ctx.Done():
		}
	}()
	for {
		select {
		case <-s.stopCh:
			return
		case ev := <-s.queue:
			s.deliver(ctx, ev)
		}
	}
}

// Notify enqueues an event without blocking. It returns false when the service
// is disabled per the cached config or the queue is full; callers never retry.
func (s *AdminNotifyService) Notify(ev AdminNotifyEvent) bool {
	if s == nil || ev.Type == "" {
		return false
	}
	return s.tryEnqueue(ev)
}

// tryEnqueue applies the disabled-cache gate and enqueues without blocking.
// Safe to call with cooldownMu held: it only touches cfgMu (never the reverse
// order elsewhere), and the channel send is non-blocking.
func (s *AdminNotifyService) tryEnqueue(ev AdminNotifyEvent) bool {
	// Cheap cache-only gate: avoid filling the queue while disabled. A stale
	// cache at most delays the first event after enabling by the TTL.
	s.cfgMu.Lock()
	cached := s.cachedCfg
	s.cfgMu.Unlock()
	if cached != nil && !cached.Enabled {
		return false
	}
	select {
	case s.queue <- ev:
		return true
	default:
		slog.Warn("admin_notify_queue_full_dropped", "event_type", ev.Type)
		return false
	}
}

// NotifyCooldown enqueues an event at most once per cooldown window per key.
// Intended for high-frequency hot-path events (e.g. quota auto-pause vetoes).
// Check, enqueue and record happen under one mutex hold so that concurrent
// requests cannot duplicate the card, and the cooldown is only consumed when
// the event actually reached the queue — a disabled config or a full queue
// must not silence the account for the whole window after conditions recover.
//
// The cooldown is booked at enqueue time, not on delivery success: accounting
// only after a successful send would let a down webhook turn every hot-path
// veto into another enqueue until the bounded queue fills and crowds out
// order/breaker events. A failed delivery costs at most one card per window
// per account; the next occurrence after the window expires retries it.
func (s *AdminNotifyService) NotifyCooldown(key string, cooldown time.Duration, ev AdminNotifyEvent) bool {
	if s == nil || key == "" {
		return false
	}
	now := time.Now()
	s.cooldownMu.Lock()
	defer s.cooldownMu.Unlock()
	if lastAt, ok := s.cooldownSentAt[key]; ok {
		if elapsed := now.Sub(lastAt); elapsed >= 0 && elapsed < cooldown {
			return false
		}
	}
	if !s.tryEnqueue(ev) {
		return false
	}
	s.cooldownSentAt[key] = now
	return true
}

// deliver sends one event using the worker context (cancellable via stopCh) so
// shutdown aborts in-flight deliveries; the per-event timeout is layered on top.
func (s *AdminNotifyService) deliver(ctx context.Context, ev AdminNotifyEvent) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("admin_notify_worker_panic", "event_type", ev.Type, "panic", r)
		}
	}()

	ctx, cancel := context.WithTimeout(ctx, adminNotifySendTimeout)
	defer cancel()

	cfg, err := s.getRuntimeConfig(ctx)
	if err != nil {
		slog.Warn("admin_notify_config_load_failed", "event_type", ev.Type, "err", err.Error())
		return
	}
	if !cfg.Enabled || cfg.WebhookURL == "" || !adminNotifyEventEnabled(cfg, ev.Type) {
		return
	}
	s.limiter.SetLimit(cfg.RateLimitPerMinute)
	if !s.limiter.Allow(time.Now().UTC()) {
		slog.Warn("admin_notify_rate_limited_dropped", "event_type", ev.Type)
		return
	}
	if err := s.httpClient.sendFeishu(ctx, cfg.WebhookURL, cfg.Secret, cfg.AtUserIDs, ev); err != nil {
		slog.Warn("admin_notify_send_failed", "event_type", ev.Type, "err", err.Error())
	}
}

func adminNotifyEventEnabled(cfg *AdminWebhookNotifyConfig, eventType string) bool {
	switch eventType {
	case AdminNotifyEventOrderRechargePaid:
		return cfg.Events.OrderRechargePaid
	case AdminNotifyEventOrderSubscriptionPaid:
		return cfg.Events.OrderSubscriptionPaid
	case AdminNotifyEventAccountAutoPaused:
		return cfg.Events.AccountAutoPaused
	case AdminNotifyEventAPIKeyBreakerTripped:
		return cfg.Events.APIKeyBreakerTripped
	case AdminNotifyEventTest:
		return true
	default:
		return false
	}
}

// ---- Config persistence (mirrors OpsService email notification config) ----

// GetConfig returns the stored config with the secret redacted for API responses.
func (s *AdminNotifyService) GetConfig(ctx context.Context) (*AdminWebhookNotifyConfigView, error) {
	cfg, err := s.loadStoredConfig(ctx)
	if err != nil {
		return nil, err
	}
	return &AdminWebhookNotifyConfigView{
		Enabled:            cfg.Enabled,
		Provider:           cfg.Provider,
		WebhookURL:         cfg.WebhookURL,
		SecretConfigured:   strings.TrimSpace(cfg.Secret) != "",
		RateLimitPerMinute: cfg.RateLimitPerMinute,
		AtUserIDs:          cfg.AtUserIDs,
		Events:             cfg.Events,
	}, nil
}

// UpdateConfig merges the partial request into the stored config. An empty
// secret keeps the stored one (write-only semantics, like the SMTP password).
func (s *AdminNotifyService) UpdateConfig(ctx context.Context, req *AdminWebhookNotifyConfigUpdateRequest) (*AdminWebhookNotifyConfigView, error) {
	if s == nil || s.settingRepo == nil {
		return nil, errors.New("setting repository not initialized")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if req == nil {
		return nil, errors.New("invalid request")
	}

	cfg, err := s.loadStoredConfig(ctx)
	if err != nil {
		return nil, &AdminNotifyInternalError{err: err}
	}

	if req.Enabled != nil {
		cfg.Enabled = *req.Enabled
	}
	if req.WebhookURL != nil {
		cfg.WebhookURL = strings.TrimSpace(*req.WebhookURL)
	}
	if req.Secret != nil {
		if trimmed := strings.TrimSpace(*req.Secret); trimmed != "" {
			cfg.Secret = trimmed
		}
	}
	// Explicit clear beats an empty secret, so admins can switch to an
	// unsigned bot (empty secret alone means "keep the stored one").
	if req.ClearSecret != nil && *req.ClearSecret {
		cfg.Secret = ""
	}
	if req.RateLimitPerMinute != nil {
		cfg.RateLimitPerMinute = *req.RateLimitPerMinute
	}
	if req.AtUserIDs != nil {
		cfg.AtUserIDs = *req.AtUserIDs
	}
	if req.Events != nil {
		cfg.Events = *req.Events
	}

	if err := validateAdminWebhookNotifyConfig(cfg); err != nil {
		return nil, err
	}
	normalizeAdminWebhookNotifyConfig(cfg)

	raw, err := json.Marshal(cfg)
	if err != nil {
		return nil, &AdminNotifyInternalError{err: err}
	}
	if err := s.settingRepo.Set(ctx, SettingKeyAdminWebhookNotifyConfig, string(raw)); err != nil {
		return nil, &AdminNotifyInternalError{err: err}
	}
	s.InvalidateConfig()

	return &AdminWebhookNotifyConfigView{
		Enabled:            cfg.Enabled,
		Provider:           cfg.Provider,
		WebhookURL:         cfg.WebhookURL,
		SecretConfigured:   strings.TrimSpace(cfg.Secret) != "",
		RateLimitPerMinute: cfg.RateLimitPerMinute,
		AtUserIDs:          cfg.AtUserIDs,
		Events:             cfg.Events,
	}, nil
}

// AdminNotifyValidationError marks user-input problems in config operations;
// handlers map it to 400 and may echo its message. Anything else is
// infrastructure-side and must not be echoed to clients.
type AdminNotifyValidationError struct{ msg string }

func (e *AdminNotifyValidationError) Error() string { return e.msg }

// AdminNotifyInternalError marks infrastructure failures (settings storage,
// etc.); handlers map it to 500 with a generic message and log the cause.
type AdminNotifyInternalError struct{ err error }

func (e *AdminNotifyInternalError) Error() string { return e.err.Error() }
func (e *AdminNotifyInternalError) Unwrap() error { return e.err }

// at_user_ids are spliced into lark_md <at> tags; restrict to Feishu open_id
// style tokens so a malformed entry cannot inject card markup.
var adminNotifyAtUserIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func validateAdminWebhookNotifyConfig(cfg *AdminWebhookNotifyConfig) error {
	if cfg == nil {
		return &AdminNotifyValidationError{msg: "invalid config"}
	}
	if cfg.WebhookURL != "" {
		u, err := url.Parse(cfg.WebhookURL)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return &AdminNotifyValidationError{msg: "webhook url must be a valid https URL"}
		}
	}
	if cfg.Enabled && cfg.WebhookURL == "" {
		return &AdminNotifyValidationError{msg: "webhook url is required when notifications are enabled"}
	}
	for _, uid := range cfg.AtUserIDs {
		if !adminNotifyAtUserIDPattern.MatchString(uid) {
			return &AdminNotifyValidationError{msg: fmt.Sprintf("invalid at_user_ids entry %q: only letters, digits, underscore and dash are allowed", truncateString(uid, 32))}
		}
	}
	return nil
}

// InvalidateConfig drops the cached config so the next read reloads from storage.
func (s *AdminNotifyService) InvalidateConfig() {
	if s == nil {
		return
	}
	s.cfgMu.Lock()
	s.cachedCfg = nil
	s.cachedAt = time.Time{}
	s.cfgMu.Unlock()
}

// getRuntimeConfig returns the full config (secret included) with a small TTL
// cache so the worker does not hit the settings table per event.
func (s *AdminNotifyService) getRuntimeConfig(ctx context.Context) (*AdminWebhookNotifyConfig, error) {
	if s == nil || s.settingRepo == nil {
		return nil, errors.New("setting repository not initialized")
	}
	s.cfgMu.Lock()
	if s.cachedCfg != nil && time.Since(s.cachedAt) < adminNotifyConfigTTLC {
		cfg := *s.cachedCfg
		s.cfgMu.Unlock()
		return &cfg, nil
	}
	s.cfgMu.Unlock()

	cfg, err := s.loadStoredConfig(ctx)
	if err != nil {
		return nil, err
	}
	s.cfgMu.Lock()
	s.cachedCfg = cfg
	s.cachedAt = time.Now()
	s.cfgMu.Unlock()
	return cfg, nil
}

func (s *AdminNotifyService) loadStoredConfig(ctx context.Context) (*AdminWebhookNotifyConfig, error) {
	defaultCfg := defaultAdminWebhookNotifyConfig()
	if s == nil || s.settingRepo == nil {
		return defaultCfg, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}

	raw, err := s.settingRepo.GetValue(ctx, SettingKeyAdminWebhookNotifyConfig)
	if err != nil {
		if errors.Is(err, ErrSettingNotFound) {
			// Return in-memory defaults only. Writing defaults back on the read
			// path could clobber a concurrent first save (PUT) with factory
			// values; the key is created by UpdateConfig when needed.
			return defaultCfg, nil
		}
		return nil, err
	}

	cfg := &AdminWebhookNotifyConfig{}
	if err := json.Unmarshal([]byte(raw), cfg); err != nil {
		// Corrupted JSON should not break the admin UI; fall back to defaults.
		return defaultCfg, nil
	}
	normalizeAdminWebhookNotifyConfig(cfg)
	return cfg, nil
}

// SendTest delivers a test card synchronously (not via the queue) using the
// given url/secret, falling back to the stored config when either is empty.
// ignoreStoredSecret skips the secret fallback so an unsigned configuration
// can be exercised even when a stored secret exists.
func (s *AdminNotifyService) SendTest(ctx context.Context, webhookURL, secret string, atUserIDs []string, ignoreStoredSecret bool) error {
	if s == nil {
		return errors.New("admin notify service not initialized")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	stored, err := s.getRuntimeConfig(ctx)
	if err != nil {
		return &AdminNotifyInternalError{err: err}
	}
	if strings.TrimSpace(webhookURL) == "" {
		webhookURL = stored.WebhookURL
	}
	if strings.TrimSpace(secret) == "" && !ignoreStoredSecret {
		secret = stored.Secret
	}
	if atUserIDs == nil {
		atUserIDs = stored.AtUserIDs
	}
	webhookURL = strings.TrimSpace(webhookURL)
	if webhookURL == "" {
		return &AdminNotifyValidationError{msg: "webhook url is not configured"}
	}
	// at_user_ids must pass the same check as the saved config: they are spliced
	// into lark_md <at> tags, and the test endpoint must not bypass validation.
	if err := validateAdminWebhookNotifyConfig(&AdminWebhookNotifyConfig{WebhookURL: webhookURL, AtUserIDs: atUserIDs}); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, adminNotifySendTimeout)
	defer cancel()

	return s.httpClient.sendFeishu(ctx, webhookURL, secret, atUserIDs, AdminNotifyEvent{
		Type:     AdminNotifyEventTest,
		Severity: AdminNotifySeverityInfo,
		Title:    "Webhook 通知测试",
		Fields: []AdminNotifyField{
			{Label: "状态", Value: "如果你看到这条消息，说明通知渠道配置成功"},
			{Label: "时间", Value: time.Now().UTC().Format("2006-01-02 15:04:05 UTC")},
		},
	})
}

// ---- Hot-path hook ----

// adminNotifyHotPath holds the running service so package-level scheduling
// free functions (no receiver available) can emit notifications. Mirrors the
// openAIAutoResetNotifierRegistry approach.
var adminNotifyHotPath atomic.Pointer[AdminNotifyService]

// notifyAdminAccountAutoPausedFromScheduler reports a quota auto-pause veto
// from the scheduling hot path, cooled down per account.
func notifyAdminAccountAutoPausedFromScheduler(account *Account, platform string, decision openAIQuotaAutoPauseDecision) {
	svc := adminNotifyHotPath.Load()
	if svc == nil || account == nil {
		return
	}
	utilization := ""
	if decision.threshold > 0 && decision.utilization > 0 {
		utilization = fmt.Sprintf("%.0f%% / 阈值 %.0f%%", decision.utilization*100, decision.threshold*100)
	}
	svc.NotifyCooldown(
		fmt.Sprintf("quota_auto_pause:%d", account.ID),
		adminNotifyQuotaPauseCooldown,
		AdminNotifyEvent{
			Type:     AdminNotifyEventAccountAutoPaused,
			Severity: AdminNotifySeverityWarning,
			Title:    "账号配额自动暂停",
			Fields: []AdminNotifyField{
				{Label: "账号", Value: account.Name},
				{Label: "平台", Value: platform},
				{Label: "窗口", Value: decision.window},
				{Label: "用量", Value: utilization},
			},
		},
	)
}
