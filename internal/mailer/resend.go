package mailer

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/mail"
	"net/url"
	"strings"
	"time"

	"github.com/resend/resend-go/v2"
)

// ResendConfig configures sending through the Resend API (https://resend.com).
type ResendConfig struct {
	APIKey string
	// From must use a domain verified in Resend ("Painel <painel@exemplo.com>");
	// for first tests Resend allows "onboarding@resend.dev".
	From    string
	Timeout time.Duration
	// BaseURL overrides the API address (tests only).
	BaseURL string
}

// Resend sends e-mail through the Resend API using its official Go SDK.
type Resend struct {
	client  *resend.Client
	from    string
	timeout time.Duration
}

// NewResend validates the settings.
func NewResend(cfg ResendConfig) (*Resend, error) {
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, errors.New("Resend: a chave de API é obrigatória")
	}
	if cfg.From == "" {
		return nil, errors.New("Resend: o remetente é obrigatório (--mail-from)")
	}
	if _, err := mail.ParseAddress(cfg.From); err != nil {
		return nil, fmt.Errorf("Resend: remetente inválido: %w", err)
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 15 * time.Second
	}
	client := resend.NewCustomClient(&http.Client{Timeout: cfg.Timeout}, cfg.APIKey)
	if cfg.BaseURL != "" {
		u, err := url.Parse(cfg.BaseURL)
		if err != nil {
			return nil, fmt.Errorf("Resend: URL inválida: %w", err)
		}
		client.BaseURL = u
	}
	return &Resend{client: client, from: cfg.From, timeout: cfg.Timeout}, nil
}

// Send delivers one plain-text message. to must be a plain address.
func (r *Resend) Send(to, subject, body string) error {
	if !ValidAddress(to) {
		return errors.New("destinatário inválido")
	}
	if strings.ContainsAny(subject, "\r\n") {
		return errors.New("assunto inválido")
	}
	ctx, cancel := context.WithTimeout(context.Background(), r.timeout)
	defer cancel()
	if _, err := r.client.Emails.SendWithContext(ctx, &resend.SendEmailRequest{
		From: r.from, To: []string{to}, Subject: subject, Text: body,
	}); err != nil {
		return fmt.Errorf("Resend: %w", err)
	}
	return nil
}
