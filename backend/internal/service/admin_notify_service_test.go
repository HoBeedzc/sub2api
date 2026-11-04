//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	dbent "github.com/Wei-Shaw/sub2api/ent"
)

// adminNotifySettingRepo mirrors the real SettingRepository semantics for
// missing keys (ErrSettingNotFound), unlike the shared mockSettingRepo which
// returns ("", nil).
type adminNotifySettingRepo struct {
	mockSettingRepo
}

func newAdminNotifySettingRepo() *adminNotifySettingRepo {
	return &adminNotifySettingRepo{mockSettingRepo{data: map[string]string{}}}
}

func (m *adminNotifySettingRepo) GetValue(_ context.Context, key string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.data[key]
	if !ok {
		return "", ErrSettingNotFound
	}
	return v, nil
}

// ---------- config load / update ----------

func TestAdminNotifyLoadStoredConfig_DefaultsAndSeed(t *testing.T) {
	repo := newAdminNotifySettingRepo()
	svc := NewAdminNotifyService(repo)

	cfg, err := svc.loadStoredConfig(context.Background())
	require.NoError(t, err)
	require.Equal(t, adminWebhookNotifyProviderFeishu, cfg.Provider)
	require.False(t, cfg.Enabled)
	require.Equal(t, adminWebhookNotifyRateLimitDefault, cfg.RateLimitPerMinute)
	require.True(t, cfg.Events.OrderRechargePaid)
	require.True(t, cfg.Events.AccountAutoPaused)

	// Missing key must NOT be seeded from the read path: a default write-back
	// racing a concurrent first save could clobber the just-saved config.
	require.NotContains(t, repo.data, SettingKeyAdminWebhookNotifyConfig)
}

func TestAdminNotifyLoadStoredConfig_CorruptedJSONFallsBack(t *testing.T) {
	repo := newAdminNotifySettingRepo()
	repo.data[SettingKeyAdminWebhookNotifyConfig] = "{not-json"
	svc := NewAdminNotifyService(repo)

	cfg, err := svc.loadStoredConfig(context.Background())
	require.NoError(t, err)
	require.Equal(t, adminWebhookNotifyRateLimitDefault, cfg.RateLimitPerMinute)
}

func TestAdminNotifyGetConfig_RedactsSecret(t *testing.T) {
	repo := newAdminNotifySettingRepo()
	stored := defaultAdminWebhookNotifyConfig()
	stored.Secret = "s3cr3t"
	stored.WebhookURL = "https://open.feishu.cn/hook/x"
	raw, _ := json.Marshal(stored)
	repo.data[SettingKeyAdminWebhookNotifyConfig] = string(raw)
	svc := NewAdminNotifyService(repo)

	view, err := svc.GetConfig(context.Background())
	require.NoError(t, err)
	require.True(t, view.SecretConfigured)
	viewJSON, err := json.Marshal(view)
	require.NoError(t, err)
	require.NotContains(t, string(viewJSON), "s3cr3t")
}

func TestAdminNotifyUpdateConfig_EmptySecretKeepsStored(t *testing.T) {
	repo := newAdminNotifySettingRepo()
	stored := defaultAdminWebhookNotifyConfig()
	stored.Secret = "keep-me"
	stored.WebhookURL = "https://open.feishu.cn/hook/x"
	raw, _ := json.Marshal(stored)
	repo.data[SettingKeyAdminWebhookNotifyConfig] = string(raw)
	svc := NewAdminNotifyService(repo)

	empty := ""
	enabled := true
	url := "https://open.feishu.cn/hook/y"
	view, err := svc.UpdateConfig(context.Background(), &AdminWebhookNotifyConfigUpdateRequest{
		Enabled:    &enabled,
		WebhookURL: &url,
		Secret:     &empty,
	})
	require.NoError(t, err)
	require.True(t, view.Enabled)
	require.Equal(t, url, view.WebhookURL)
	require.True(t, view.SecretConfigured)

	// Stored secret survived.
	cfg, err := svc.loadStoredConfig(context.Background())
	require.NoError(t, err)
	require.Equal(t, "keep-me", cfg.Secret)
	// Cache invalidated by update.
	require.Nil(t, svc.cachedCfg)
}

func TestAdminNotifyUpdateConfig_Validation(t *testing.T) {
	svc := NewAdminNotifyService(newAdminNotifySettingRepo())

	enabled := true
	_, err := svc.UpdateConfig(context.Background(), &AdminWebhookNotifyConfigUpdateRequest{Enabled: &enabled})
	require.Error(t, err) // enabled without url

	badURL := "http://open.feishu.cn/hook/x"
	_, err = svc.UpdateConfig(context.Background(), &AdminWebhookNotifyConfigUpdateRequest{WebhookURL: &badURL})
	require.Error(t, err) // not https

	rate := 500
	view, err := svc.UpdateConfig(context.Background(), &AdminWebhookNotifyConfigUpdateRequest{RateLimitPerMinute: &rate})
	require.NoError(t, err)
	require.Equal(t, adminWebhookNotifyRateLimitMax, view.RateLimitPerMinute) // clamped
}

// ---------- queue / cooldown ----------

func TestAdminNotifyNotify_QueueFullDrops(t *testing.T) {
	svc := NewAdminNotifyService(newAdminNotifySettingRepo())
	ev := AdminNotifyEvent{Type: AdminNotifyEventOrderRechargePaid, Severity: AdminNotifySeverityInfo}
	for i := 0; i < adminNotifyQueueCapacity; i++ {
		require.True(t, svc.Notify(ev))
	}
	require.False(t, svc.Notify(ev)) // dropped, no block
}

func TestAdminNotifyNotify_DisabledCacheGate(t *testing.T) {
	repo := newAdminNotifySettingRepo()
	cfg := defaultAdminWebhookNotifyConfig() // Enabled=false
	raw, _ := json.Marshal(cfg)
	repo.data[SettingKeyAdminWebhookNotifyConfig] = string(raw)
	svc := NewAdminNotifyService(repo)

	// Prime the runtime cache, then Notify must short-circuit.
	_, err := svc.getRuntimeConfig(context.Background())
	require.NoError(t, err)
	require.False(t, svc.Notify(AdminNotifyEvent{Type: AdminNotifyEventOrderRechargePaid}))
}

func TestAdminNotifyNotifyCooldown(t *testing.T) {
	svc := NewAdminNotifyService(newAdminNotifySettingRepo())
	ev := AdminNotifyEvent{Type: AdminNotifyEventAccountAutoPaused, Severity: AdminNotifySeverityWarning}

	require.True(t, svc.NotifyCooldown("k", time.Hour, ev))
	require.False(t, svc.NotifyCooldown("k", time.Hour, ev))
	// Different key unaffected.
	require.True(t, svc.NotifyCooldown("k2", time.Hour, ev))
}

// ---------- feishu sender ----------

func TestSignFeishu_KnownVector(t *testing.T) {
	// HMAC-SHA256(key = "1700000000\ntest-secret", msg = "") → base64.
	require.Equal(t, "mbm4Y4oluIPQ00qlBIhX8vAZ0EKv3nw0LuTb91jPL84=", signFeishu("test-secret", 1700000000))
}

func TestBuildFeishuCardMessage_Shape(t *testing.T) {
	ev := AdminNotifyEvent{
		Type:     AdminNotifyEventTest,
		Severity: AdminNotifySeverityCritical,
		Title:    "T",
		Fields:   []AdminNotifyField{{Label: "L", Value: "V"}},
	}
	raw, err := buildFeishuCardMessage("sec", []string{"ou_1"}, ev, 1700000000)
	require.NoError(t, err)

	var body map[string]any
	require.NoError(t, json.Unmarshal(raw, &body))
	require.Equal(t, "interactive", body["msg_type"])
	require.Equal(t, "1700000000", body["timestamp"])
	require.NotEmpty(t, body["sign"])

	card := body["card"].(map[string]any)
	header := card["header"].(map[string]any)
	require.Equal(t, "red", header["template"])
	require.Equal(t, "T", header["title"].(map[string]any)["content"])
}

// newFeishuTestServer records request bodies and signals each arrival. It uses
// TLS because the config validator only accepts https webhook URLs.
func newFeishuTestServer(t *testing.T) (*httptest.Server, *sync.Map, chan struct{}) {
	t.Helper()
	var bodies sync.Map
	arrived := make(chan struct{}, 16)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		bodies.Store(strconv.Itoa(int(time.Now().UnixNano())), string(raw))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"msg":"success"}`))
		arrived <- struct{}{}
	}))
	t.Cleanup(srv.Close)
	return srv, &bodies, arrived
}

// trustTestServer swaps the service HTTP client for one that trusts the
// httptest TLS certificate.
func trustTestServer(svc *AdminNotifyService, srv *httptest.Server) {
	svc.httpClient = &adminNotifyHTTPClient{client: srv.Client()}
}

// enableAdminNotify primes a stored enabled config pointing at srv.URL.
func enableAdminNotify(repo *adminNotifySettingRepo, url string, events AdminWebhookNotifyEvents) {
	cfg := defaultAdminWebhookNotifyConfig()
	cfg.Enabled = true
	cfg.WebhookURL = url
	if (events == AdminWebhookNotifyEvents{}) {
		events = defaultAdminWebhookNotifyConfig().Events
	}
	cfg.Events = events
	raw, _ := json.Marshal(cfg)
	repo.data[SettingKeyAdminWebhookNotifyConfig] = string(raw)
}

func TestAdminNotifyDeliver_SendsCard(t *testing.T) {
	srv, bodies, arrived := newFeishuTestServer(t)
	repo := newAdminNotifySettingRepo()
	enableAdminNotify(repo, srv.URL, AdminWebhookNotifyEvents{})
	svc := NewAdminNotifyService(repo)
	trustTestServer(svc, srv)
	svc.Start()
	defer svc.Stop()

	require.True(t, svc.Notify(AdminNotifyEvent{
		Type:     AdminNotifyEventOrderRechargePaid,
		Severity: AdminNotifySeverityInfo,
		Title:    "order",
		Fields:   []AdminNotifyField{{Label: "订单号", Value: "42"}},
	}))

	select {
	case <-arrived:
	case <-time.After(3 * time.Second):
		t.Fatal("card not delivered")
	}
	var payload string
	bodies.Range(func(_, v any) bool { payload = v.(string); return false })
	require.Contains(t, payload, `"msg_type":"interactive"`)
	require.Contains(t, payload, "42")
}

func TestAdminNotifyDeliver_EventToggledOff(t *testing.T) {
	srv, _, arrived := newFeishuTestServer(t)
	repo := newAdminNotifySettingRepo()
	events := defaultAdminWebhookNotifyConfig().Events
	events.AccountAutoPaused = false
	enableAdminNotify(repo, srv.URL, events)
	svc := NewAdminNotifyService(repo)
	trustTestServer(svc, srv)
	svc.Start()
	defer svc.Stop()

	require.True(t, svc.Notify(AdminNotifyEvent{Type: AdminNotifyEventAccountAutoPaused, Severity: AdminNotifySeverityWarning, Title: "x"}))

	select {
	case <-arrived:
		t.Fatal("event should be dropped by toggle")
	case <-time.After(500 * time.Millisecond):
	}
}

func TestAdminNotifySendFeishu_NetworkErrorDoesNotLeakWebhookURL(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	urlWithToken := srv.URL + "/hook/SECRET_TOKEN_123"
	srv.Close() // force a transport error on every attempt

	client := newAdminNotifyHTTPClient(500 * time.Millisecond)
	err := client.sendFeishu(context.Background(), urlWithToken, "", nil,
		AdminNotifyEvent{Type: AdminNotifyEventTest, Severity: AdminNotifySeverityInfo, Title: "t"})
	require.Error(t, err)
	require.NotContains(t, err.Error(), "SECRET_TOKEN_123")
	require.NotContains(t, err.Error(), srv.URL)
}

// collectCardTexts walks card elements and returns (larkMd, plainText) contents.
func collectCardTexts(body map[string]any) (larkMd []string, plainText []string) {
	card, _ := body["card"].(map[string]any)
	elements, _ := card["elements"].([]any)
	for _, el := range elements {
		div, _ := el.(map[string]any)
		fields, _ := div["fields"].([]any)
		for _, f := range fields {
			field, _ := f.(map[string]any)
			text, _ := field["text"].(map[string]any)
			content, _ := text["content"].(string)
			switch text["tag"] {
			case "lark_md":
				larkMd = append(larkMd, content)
			case "plain_text":
				plainText = append(plainText, content)
			}
		}
	}
	return larkMd, plainText
}

func TestBuildFeishuCardMessage_InjectionStaysPlainText(t *testing.T) {
	malicious := `<at user_id="all"></at> [点我](https://evil.example)`
	ev := AdminNotifyEvent{
		Type:     AdminNotifyEventOrderRechargePaid,
		Severity: AdminNotifySeverityInfo,
		Title:    "t",
		Fields:   []AdminNotifyField{{Label: "用户", Value: malicious}},
	}
	raw, err := buildFeishuCardMessage("", nil, ev, 1700000000)
	require.NoError(t, err)

	var body map[string]any
	require.NoError(t, json.Unmarshal(raw, &body))
	larkMd, plainText := collectCardTexts(body)

	for _, md := range larkMd {
		require.NotContains(t, md, "<at", "user input must not reach lark_md")
		require.NotContains(t, md, "evil.example")
	}
	found := false
	for _, pt := range plainText {
		if pt == malicious {
			found = true
		}
	}
	require.True(t, found, "value must render verbatim in plain_text")
}

func TestAdminNotifyNotifyCooldown_ConcurrentSameKeyAllowsOne(t *testing.T) {
	svc := NewAdminNotifyService(newAdminNotifySettingRepo())
	ev := AdminNotifyEvent{Type: AdminNotifyEventAccountAutoPaused, Severity: AdminNotifySeverityWarning}

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			svc.NotifyCooldown("k", time.Hour, ev)
		}()
	}
	wg.Wait()
	require.Equal(t, 1, len(svc.queue))
}

// A drop while the cached config is disabled must not consume the cooldown:
// once the admin enables notifications, the very next event for the same key
// has to reach the queue.
func TestAdminNotifyNotifyCooldown_DisabledThenEnabledRecovers(t *testing.T) {
	repo := newAdminNotifySettingRepo()
	cfg := defaultAdminWebhookNotifyConfig() // Enabled=false
	raw, _ := json.Marshal(cfg)
	repo.data[SettingKeyAdminWebhookNotifyConfig] = string(raw)
	svc := NewAdminNotifyService(repo)
	ev := AdminNotifyEvent{Type: AdminNotifyEventAccountAutoPaused, Severity: AdminNotifySeverityWarning}

	_, err := svc.getRuntimeConfig(context.Background()) // prime the disabled cache
	require.NoError(t, err)
	require.False(t, svc.NotifyCooldown("k", time.Hour, ev))

	enabled := defaultAdminWebhookNotifyConfig()
	enabled.Enabled = true
	raw, _ = json.Marshal(enabled)
	repo.data[SettingKeyAdminWebhookNotifyConfig] = string(raw)
	svc.InvalidateConfig()

	require.True(t, svc.NotifyCooldown("k", time.Hour, ev), "cooldown must not be consumed by the disabled drop")
	require.Equal(t, 1, len(svc.queue))
}

// A drop on a full queue must not consume the cooldown either.
func TestAdminNotifyNotifyCooldown_QueueFullThenFreedRecovers(t *testing.T) {
	svc := NewAdminNotifyService(newAdminNotifySettingRepo())
	ev := AdminNotifyEvent{Type: AdminNotifyEventAccountAutoPaused, Severity: AdminNotifySeverityWarning}

	for i := 0; i < adminNotifyQueueCapacity; i++ {
		require.True(t, svc.Notify(ev))
	}
	require.False(t, svc.NotifyCooldown("k", time.Hour, ev))

	<-svc.queue // free one slot
	require.True(t, svc.NotifyCooldown("k", time.Hour, ev), "cooldown must not be consumed by the full-queue drop")
}

func TestAdminNotifySendFeishu_AllAttemptsRunWithinBudget(t *testing.T) {
	var attempts atomic.Int32
	// Each response hangs longer than the client's per-attempt timeout so every
	// attempt times out client-side. Bounded (rather than waiting on
	// r.Context().Done()) because the httptest client cancel does not reliably
	// propagate to the server-side context over HTTP/2, and Server.Close waits
	// for handlers to return.
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		time.Sleep(300 * time.Millisecond)
	}))
	t.Cleanup(srv.Close)

	restore := func(a, b time.Duration) { adminNotifyFeishuBackoffFirst, adminNotifyFeishuBackoffSecond = a, b }
	defer restore(adminNotifyFeishuBackoffFirst, adminNotifyFeishuBackoffSecond)
	adminNotifyFeishuBackoffFirst, adminNotifyFeishuBackoffSecond = 5*time.Millisecond, 5*time.Millisecond

	tlsClient := srv.Client()
	tlsClient.Timeout = 80 * time.Millisecond
	client := &adminNotifyHTTPClient{client: tlsClient}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := client.sendFeishu(ctx, srv.URL, "", nil,
		AdminNotifyEvent{Type: AdminNotifyEventTest, Severity: AdminNotifySeverityInfo, Title: "t"})
	require.Error(t, err)
	require.Equal(t, int32(1+adminNotifyFeishuMaxRetries), attempts.Load(),
		"budget must accommodate every retry attempt")
}

func TestAdminNotifyUpdateConfig_ClearSecret(t *testing.T) {
	repo := newAdminNotifySettingRepo()
	stored := defaultAdminWebhookNotifyConfig()
	stored.Secret = "old-secret"
	stored.WebhookURL = "https://open.feishu.cn/hook/x"
	raw, _ := json.Marshal(stored)
	repo.data[SettingKeyAdminWebhookNotifyConfig] = string(raw)
	svc := NewAdminNotifyService(repo)

	clear := true
	view, err := svc.UpdateConfig(context.Background(), &AdminWebhookNotifyConfigUpdateRequest{ClearSecret: &clear})
	require.NoError(t, err)
	require.False(t, view.SecretConfigured)

	cfg, err := svc.loadStoredConfig(context.Background())
	require.NoError(t, err)
	require.Empty(t, cfg.Secret)
}

func TestAdminNotifySendTest_IgnoreStoredSecretSendsUnsigned(t *testing.T) {
	var bodies []string
	var mu sync.Mutex
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(raw))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"msg":"success"}`))
	}))
	t.Cleanup(srv.Close)

	repo := newAdminNotifySettingRepo()
	cfg := defaultAdminWebhookNotifyConfig()
	cfg.WebhookURL = srv.URL
	cfg.Secret = "stored-secret"
	raw, _ := json.Marshal(cfg)
	repo.data[SettingKeyAdminWebhookNotifyConfig] = string(raw)
	svc := NewAdminNotifyService(repo)
	trustTestServer(svc, srv)

	require.NoError(t, svc.SendTest(context.Background(), "", "", nil, true))
	mu.Lock()
	defer mu.Unlock()
	require.Len(t, bodies, 1)
	require.NotContains(t, bodies[0], `"sign"`)
}

func TestAdminNotifyDeliver_FeishuBusinessErrorNoRetry(t *testing.T) {
	var attempts int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":19021,"msg":"sign match fail"}`))
	}))
	t.Cleanup(srv.Close)

	repo := newAdminNotifySettingRepo()
	enableAdminNotify(repo, srv.URL, AdminWebhookNotifyEvents{})
	svc := NewAdminNotifyService(repo)
	trustTestServer(svc, srv)
	svc.Start()
	defer svc.Stop()

	svc.Notify(AdminNotifyEvent{Type: AdminNotifyEventTest, Severity: AdminNotifySeverityInfo, Title: "t"})
	time.Sleep(500 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, 1, attempts, "business error must not be retried")
}

// ---------- SendTest ----------

func TestAdminNotifySendTest_FallsBackToStoredConfig(t *testing.T) {
	srv, _, arrived := newFeishuTestServer(t)
	repo := newAdminNotifySettingRepo()
	enableAdminNotify(repo, srv.URL, AdminWebhookNotifyEvents{})
	svc := NewAdminNotifyService(repo)
	trustTestServer(svc, srv)

	require.NoError(t, svc.SendTest(context.Background(), "", "", nil, false))
	select {
	case <-arrived:
	case <-time.After(3 * time.Second):
		t.Fatal("test card not delivered")
	}
}

// The test endpoint feeds at_user_ids into lark_md just like the saved config;
// malformed entries must be rejected here too, not only on save.
func TestAdminNotifySendTest_RejectsInvalidAtUserIDs(t *testing.T) {
	svc := NewAdminNotifyService(newAdminNotifySettingRepo())
	err := svc.SendTest(context.Background(), "https://open.feishu.cn/hook/x", "", []string{`ou_bad"><at`}, false)
	var validationErr *AdminNotifyValidationError
	require.ErrorAs(t, err, &validationErr)
}

func TestAdminNotifySendTest_NoURLConfigured(t *testing.T) {
	svc := NewAdminNotifyService(newAdminNotifySettingRepo())
	require.Error(t, svc.SendTest(context.Background(), "", "", nil, false))
}

// ---------- hook points ----------

func TestNotifyAdminWebhookOrderPaid_EnqueuesBothTypes(t *testing.T) {
	repo := newAdminNotifySettingRepo()
	notify := NewAdminNotifyService(repo)
	ps := &PaymentService{adminNotifyService: notify}

	ps.notifyAdminWebhookOrderPaid(&dbent.PaymentOrder{ID: 7, Amount: 12.5, PayAmount: 88.0, UserEmail: "u@x.com", PaymentType: "alipay"}, "RECHARGE_SUCCESS")
	ps.notifyAdminWebhookOrderPaid(&dbent.PaymentOrder{ID: 8, Amount: 30, PayAmount: 199.0, UserEmail: "u@x.com", SubscriptionDays: nil}, "SUBSCRIPTION_SUCCESS")
	// Unknown action enqueues nothing.
	ps.notifyAdminWebhookOrderPaid(&dbent.PaymentOrder{ID: 9}, "ORDER_PAID")

	require.Equal(t, 2, len(notify.queue))
	first := <-notify.queue
	require.Equal(t, AdminNotifyEventOrderRechargePaid, first.Type)
	require.Equal(t, AdminNotifySeverityInfo, first.Severity)
	// Both amount columns must be present and distinctly labelled.
	require.Contains(t, fieldLabels(first), "到账金额")
	require.Contains(t, fieldLabels(first), "实付金额")
	require.Equal(t, "12.50", fieldValue(first, "到账金额"))
	require.Equal(t, "88.00", fieldValue(first, "实付金额"))
	second := <-notify.queue
	require.Equal(t, AdminNotifyEventOrderSubscriptionPaid, second.Type)
	require.Equal(t, "199.00", fieldValue(second, "实付金额"))
}

func fieldLabels(ev AdminNotifyEvent) []string {
	labels := make([]string, 0, len(ev.Fields))
	for _, f := range ev.Fields {
		labels = append(labels, f.Label)
	}
	return labels
}

func fieldValue(ev AdminNotifyEvent, label string) string {
	for _, f := range ev.Fields {
		if f.Label == label {
			return f.Value
		}
	}
	return ""
}

func TestNotifyAdminAccountAutoPausedFromScheduler_CooldownPerAccount(t *testing.T) {
	repo := newAdminNotifySettingRepo()
	svc := NewAdminNotifyService(repo)
	prev := adminNotifyHotPath.Swap(svc)
	t.Cleanup(func() { adminNotifyHotPath.Store(prev) })

	acc := &Account{ID: 5, Name: "acc-5", Platform: "openai"}
	decision := openAIQuotaAutoPauseDecision{window: "5h", threshold: 0.9, utilization: 0.95}

	notifyAdminAccountAutoPausedFromScheduler(acc, "openai", decision)
	require.Equal(t, 1, len(svc.queue))
	ev := <-svc.queue
	require.Equal(t, AdminNotifyEventAccountAutoPaused, ev.Type)
	require.Equal(t, "acc-5", ev.Fields[0].Value)

	// Second call within the cooldown window is dropped before enqueue.
	notifyAdminAccountAutoPausedFromScheduler(acc, "openai", decision)
	require.Equal(t, 0, len(svc.queue))
}

// failingSetSettingRepo simulates a settings-storage outage on write.
type failingSetSettingRepo struct {
	adminNotifySettingRepo
	setErr error
}

func (m *failingSetSettingRepo) Set(_ context.Context, key, value string) error {
	return m.setErr
}

// ---------- error classification (handler maps 400 vs 500 on these) ----------

func TestAdminNotifyUpdateConfig_ErrorClassification(t *testing.T) {
	// Validation failure: typed, safe to echo.
	svc := NewAdminNotifyService(newAdminNotifySettingRepo())
	enabled := true
	_, err := svc.UpdateConfig(context.Background(), &AdminWebhookNotifyConfigUpdateRequest{Enabled: &enabled})
	var validationErr *AdminNotifyValidationError
	require.ErrorAs(t, err, &validationErr)

	// Storage failure: typed as internal, must not be echoed by the handler.
	failing := &failingSetSettingRepo{setErr: errors.New("db down")}
	failing.data = map[string]string{}
	svc2 := NewAdminNotifyService(failing)
	url := "https://open.feishu.cn/hook/x"
	_, err = svc2.UpdateConfig(context.Background(), &AdminWebhookNotifyConfigUpdateRequest{WebhookURL: &url})
	var internalErr *AdminNotifyInternalError
	require.ErrorAs(t, err, &internalErr)
	require.ErrorContains(t, internalErr, "db down") // logged server-side, never echoed
}

func TestAdminNotifyUpdateConfig_RejectsInvalidAtUserIDs(t *testing.T) {
	svc := NewAdminNotifyService(newAdminNotifySettingRepo())
	ids := []string{"ou_ok", `ou_bad"><at`}
	_, err := svc.UpdateConfig(context.Background(), &AdminWebhookNotifyConfigUpdateRequest{AtUserIDs: &ids})
	var validationErr *AdminNotifyValidationError
	require.ErrorAs(t, err, &validationErr)
	require.ErrorContains(t, validationErr, "at_user_ids")
}

// ---------- quota auto-pause wrapper notifies on every scheduling path ----------

// The advanced scheduler and WS forwarder call shouldAutoPauseOpenAIAccountByQuota
// directly; the wrapper must notify (with cooldown) so those paths reach the
// admin feed just like the compat scheduling path.
func TestShouldAutoPauseOpenAIAccountByQuota_NotifiesAdminWebhook(t *testing.T) {
	now := time.Now().UTC()
	account := &Account{
		ID:       9_900_100,
		Name:     "sched-acc",
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Extra: map[string]any{
			"auto_pause_5h_threshold": 0.8,
			"codex_5h_used_percent":   90.0,
			"codex_usage_updated_at":  now.Format(time.RFC3339),
			"codex_5h_reset_at":       now.Add(time.Hour).Format(time.RFC3339),
		},
	}

	svc := NewAdminNotifyService(newAdminNotifySettingRepo())
	prev := adminNotifyHotPath.Swap(svc)
	t.Cleanup(func() { adminNotifyHotPath.Store(prev) })

	paused, decision := shouldAutoPauseOpenAIAccountByQuota(context.Background(), account)
	require.True(t, paused)
	require.Equal(t, "5h", decision.window)

	require.Equal(t, 1, len(svc.queue))
	ev := <-svc.queue
	require.Equal(t, AdminNotifyEventAccountAutoPaused, ev.Type)

	// Second evaluation inside the cooldown window: still paused, no duplicate card.
	paused, _ = shouldAutoPauseOpenAIAccountByQuota(context.Background(), account)
	require.True(t, paused)
	require.Equal(t, 0, len(svc.queue))
}

// A pause decision with the notifier unset must stay silent (nil-safe hook).
func TestShouldAutoPauseOpenAIAccountByQuota_NilNotifierSafe(t *testing.T) {
	now := time.Now().UTC()
	account := &Account{
		ID:       9_900_101,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Extra: map[string]any{
			"auto_pause_5h_threshold": 0.8,
			"codex_5h_used_percent":   90.0,
			"codex_usage_updated_at":  now.Format(time.RFC3339),
			"codex_5h_reset_at":       now.Add(time.Hour).Format(time.RFC3339),
		},
	}
	require.NotPanics(t, func() {
		paused, _ := shouldAutoPauseOpenAIAccountByQuota(context.Background(), account)
		require.True(t, paused)
	})
}

// ---------- Stop aborts in-flight delivery ----------

func TestAdminNotifyStop_AbortsInFlightDelivery(t *testing.T) {
	release := make(chan struct{})
	arrived := make(chan struct{}, 1)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		arrived <- struct{}{}
		<-release // hold the request until cleanup so only ctx cancel can end it
		_, _ = w.Write([]byte(`{"code":0}`))
	}))
	t.Cleanup(func() {
		close(release)
		srv.Close()
	})

	repo := newAdminNotifySettingRepo()
	enableAdminNotify(repo, srv.URL, AdminWebhookNotifyEvents{})
	svc := NewAdminNotifyService(repo)
	trustTestServer(svc, srv)
	svc.Start()

	require.True(t, svc.Notify(AdminNotifyEvent{Type: AdminNotifyEventOrderRechargePaid, Severity: AdminNotifySeverityInfo, Title: "t"}))
	select {
	case <-arrived:
	case <-time.After(3 * time.Second):
		t.Fatal("delivery never reached the server")
	}
	// Give the worker a beat to be inside the blocked HTTP call.
	time.Sleep(100 * time.Millisecond)

	start := time.Now()
	svc.Stop()
	elapsed := time.Since(start)
	require.Less(t, elapsed, 2*time.Second, "Stop must cancel the in-flight delivery, took %s", elapsed)
}

// ---------- redirects are never followed ----------

func TestAdminNotifySendFeishu_RedirectNotFollowed(t *testing.T) {
	var hits int
	var mu sync.Mutex
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		if r.URL.Path == "/hook" {
			w.Header().Set("Location", "/redirect-target?leak=1")
			w.WriteHeader(http.StatusFound)
			return
		}
		_, _ = w.Write([]byte(`{"code":0}`))
	}))
	t.Cleanup(srv.Close)

	client := newAdminNotifyHTTPClient(time.Second)
	// Trust the httptest self-signed CA while keeping the production client
	// (and its no-redirect policy) under test.
	client.client.Transport = srv.Client().Transport
	err := client.sendFeishu(context.Background(), srv.URL+"/hook", "", nil,
		AdminNotifyEvent{Type: AdminNotifyEventTest, Severity: AdminNotifySeverityInfo, Title: "t"})
	require.Error(t, err)

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, 1, hits, "the redirect target must not be requested")
	require.NotContains(t, err.Error(), "redirect-target", "error must not leak the redirect location")
}
