package service

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Feishu custom bot limits: 100 messages per minute per bot; we default to 50.
// See https://open.feishu.cn/document/client-docs/bot-v3/add-custom-bot
const (
	// Per-attempt HTTP timeout. Feishu webhooks answer in well under a second;
	// 5s is generous while keeping the full retry chain inside the delivery
	// budget (3 attempts + backoffs, see adminNotifySendTimeout).
	adminNotifyFeishuHTTPTimeout = 5 * time.Second
	adminNotifyFeishuMaxRetries  = 2
)

// Backoff durations between retry attempts; vars so tests can shorten them.
var (
	adminNotifyFeishuBackoffFirst  = 1 * time.Second
	adminNotifyFeishuBackoffSecond = 3 * time.Second
)

// adminNotifyRetryTotalBackoff budgets both backoffs plus slack for the
// worker-level context of a fully retried send.
const adminNotifyRetryTotalBackoff = 1*time.Second + 3*time.Second + 2*time.Second

// feishu severity -> card header template color.
var adminNotifySeverityTemplates = map[string]string{
	AdminNotifySeverityInfo:     "green",
	AdminNotifySeverityWarning:  "orange",
	AdminNotifySeverityCritical: "red",
}

// adminNotifyHTTPClient posts card messages to a Feishu custom bot webhook.
type adminNotifyHTTPClient struct {
	client *http.Client
}

func newAdminNotifyHTTPClient(timeout time.Duration) *adminNotifyHTTPClient {
	return &adminNotifyHTTPClient{client: &http.Client{
		Timeout: timeout,
		// Never follow redirects: a webhook that answers 3xx is a configuration
		// error, and following it could silently downgrade https→http or send
		// the (signed) payload to a different host. ErrUseLastResponse surfaces
		// the 3xx to sendFeishu, which treats any non-200 as a terminal error.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}}
}

// sendFeishu delivers the event as an interactive card, retrying transient
// failures (network errors, 5xx) up to twice with backoff. Feishu business
// errors (non-zero code in a 200 response) are terminal and not retried.
//
// Delivery is at-least-once: a response lost after the server processed the
// message still reads as a failure and is retried, so the group may see a
// duplicate card. Feishu custom bots expose no idempotency key, so duplicates
// are accepted rather than deduplicated.
func (c *adminNotifyHTTPClient) sendFeishu(ctx context.Context, webhookURL, secret string, atUserIDs []string, ev AdminNotifyEvent) error {
	payload, err := buildFeishuCardMessage(secret, atUserIDs, ev, time.Now().Unix())
	if err != nil {
		return err
	}

	var lastErr error
	for attempt := 0; attempt <= adminNotifyFeishuMaxRetries; attempt++ {
		if attempt > 0 {
			backoff := adminNotifyFeishuBackoffFirst
			if attempt == 2 {
				backoff = adminNotifyFeishuBackoffSecond
			}
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return fmt.Errorf("feishu send aborted: %w (last error: %v)", ctx.Err(), lastErr)
			}
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhookURL, bytes.NewReader(payload))
		if err != nil {
			return fmt.Errorf("feishu request build failed: %v", sanitizeTransportError(err))
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := c.client.Do(req)
		if err != nil {
			lastErr = sanitizeTransportError(err)
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		_ = resp.Body.Close()
		if readErr != nil {
			lastErr = readErr
			continue
		}
		if resp.StatusCode >= 500 {
			lastErr = fmt.Errorf("feishu webhook returned %d", resp.StatusCode)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("feishu webhook returned %d: %s", resp.StatusCode, truncateString(string(body), 200))
		}

		var fr struct {
			Code int    `json:"code"`
			Msg  string `json:"msg"`
		}
		if err := json.Unmarshal(body, &fr); err != nil {
			return fmt.Errorf("feishu response decode failed: %w", err)
		}
		if fr.Code != 0 {
			return fmt.Errorf("feishu error %d: %s", fr.Code, fr.Msg)
		}
		return nil
	}
	return fmt.Errorf("feishu send failed after %d attempts: %w", adminNotifyFeishuMaxRetries+1, lastErr)
}

// sanitizeTransportError strips the request URL from transport errors: the
// Feishu webhook URL embeds the bot token, which must never reach logs via
// err.Error() on DNS/connect/TLS failures (*url.Error includes the full URL).
func sanitizeTransportError(err error) error {
	if err == nil {
		return nil
	}
	var uerr *url.Error
	if errors.As(err, &uerr) {
		return uerr.Err
	}
	return err
}

// signFeishu computes the Feishu custom bot signature:
// base64(hmac_sha256(key = timestamp + "\n" + secret, data = "")).
func signFeishu(secret string, timestamp int64) string {
	stringToSign := strconv.FormatInt(timestamp, 10) + "\n" + secret
	h := hmac.New(sha256.New, []byte(stringToSign))
	// The Feishu scheme signs the empty string with the composite key.
	h.Write(nil)
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// buildFeishuCardMessage assembles the signed webhook body for an event.
func buildFeishuCardMessage(secret string, atUserIDs []string, ev AdminNotifyEvent, timestamp int64) ([]byte, error) {
	template, ok := adminNotifySeverityTemplates[ev.Severity]
	if !ok {
		template = "blue"
	}

	elements := make([]map[string]any, 0, len(ev.Fields)+3)
	for _, uid := range atUserIDs {
		if uid == "" {
			continue
		}
		elements = append(elements, map[string]any{
			"tag":  "div",
			"text": map[string]any{"tag": "lark_md", "content": "<at user_id=\"" + uid + "\"></at>"},
		})
	}

	// Field values are rendered as plain_text: they carry user-controllable
	// input (order usernames, account names) and lark_md would render injected
	// <at user_id="..."> mentions or [text](url) links. Labels are compile-time
	// constants, so they alone keep lark_md for the bold styling.
	for _, f := range ev.Fields {
		elements = append(elements, map[string]any{
			"tag": "div",
			"fields": []map[string]any{
				{"is_short": true, "text": map[string]any{"tag": "lark_md", "content": "**" + f.Label + "**"}},
				{"is_short": true, "text": map[string]any{"tag": "plain_text", "content": f.Value}},
			},
		})
	}

	elements = append(elements,
		map[string]any{"tag": "hr"},
		map[string]any{
			"tag": "note",
			"elements": []map[string]any{{
				"tag":     "plain_text",
				"content": defaultSiteName + " · " + time.Unix(timestamp, 0).UTC().Format("2006-01-02 15:04:05 UTC"),
			}},
		},
	)

	card := map[string]any{
		"config": map[string]any{"wide_screen_mode": true},
		"header": map[string]any{
			"template": template,
			"title":    map[string]any{"tag": "plain_text", "content": ev.Title},
		},
		"elements": elements,
	}

	body := map[string]any{
		"timestamp": strconv.FormatInt(timestamp, 10),
		"msg_type":  "interactive",
		"card":      card,
	}
	if secret != "" {
		body["sign"] = signFeishu(secret, timestamp)
	}
	return json.Marshal(body)
}
