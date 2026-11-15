package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"
)

// Mailer delivers a plain-text message to one address.
type Mailer interface {
	Send(ctx context.Context, to, subject, text string) error
}

// RemailMailer sends through the Remail API (https://remail.foo/docs,
// operation emails.send). It needs a sending_only key and a From address
// on a domain verified in that Remail account.
type RemailMailer struct {
	BaseURL string
	APIKey  string
	From    string
	Client  *http.Client
}

func NewRemailMailer(cfg Config) *RemailMailer {
	return &RemailMailer{BaseURL: cfg.RemailBaseURL, APIKey: cfg.RemailAPIKey, From: cfg.RemailFrom,
		Client: &http.Client{Timeout: 15 * time.Second}}
}

func (m *RemailMailer) Send(ctx context.Context, to, subject, text string) error {
	body, _ := json.Marshal(map[string]any{"from": m.From, "to": to, "subject": subject, "text": text})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.BaseURL+"/v1/emails", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+m.APIKey)
	req.Header.Set("Content-Type", "application/json")
	// A retry of the same message is not sent twice.
	req.Header.Set("Idempotency-Key", hashToken(to+"\x00"+subject+"\x00"+text))
	resp, err := m.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		// Remail errors are {"error":{"code","message","next_actions"}}; the
		// code and message say what the operator has to fix.
		var e struct {
			Error struct{ Code, Message string } `json:"error"`
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if json.Unmarshal(raw, &e) == nil && e.Error.Code != "" {
			return fmt.Errorf("remail %d %s: %s", resp.StatusCode, e.Error.Code, e.Error.Message)
		}
		return fmt.Errorf("remail responded %d", resp.StatusCode)
	}
	return nil
}

// LogMailer prints messages instead of sending them. DEV_MODE only.
type LogMailer struct{ Log *log.Logger }

func (m LogMailer) Send(_ context.Context, to, subject, text string) error {
	m.Log.Printf("DEV_MODE mail to %s: %s\n%s", to, subject, text)
	return nil
}
