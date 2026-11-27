// Package mailer sends plain-text e-mail through an SMTP server, used for the
// login PIN of the e-mail second factor.
package mailer

import (
	"crypto/tls"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/mail"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

// Security says how the connection to the SMTP server is protected.
const (
	StartTLS = "starttls" // plain connect (usually port 587), then upgrade; refused if the server cannot
	TLS      = "tls"      // TLS from the first byte (usually port 465)
	None     = "none"     // no encryption: only for a local relay
)

type Config struct {
	Host     string
	Port     int
	Username string
	Password string
	From     string // "Painel <painel@exemplo.com>" or just an address
	Security string
	Timeout  time.Duration
}

type Mailer struct {
	cfg  Config
	from mail.Address
}

// New validates the settings.
func New(cfg Config) (*Mailer, error) {
	if cfg.Host == "" || cfg.From == "" {
		return nil, errors.New("SMTP: host e remetente são obrigatórios")
	}
	if cfg.Port == 0 {
		cfg.Port = 587
	}
	switch cfg.Security {
	case "":
		cfg.Security = StartTLS
	case StartTLS, TLS, None:
	default:
		return nil, fmt.Errorf("SMTP: segurança %q inválida (use starttls, tls ou none)", cfg.Security)
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 15 * time.Second
	}
	from, err := mail.ParseAddress(cfg.From)
	if err != nil {
		return nil, fmt.Errorf("SMTP: remetente inválido: %w", err)
	}
	return &Mailer{cfg: cfg, from: *from}, nil
}

// ValidAddress reports whether s is a plain e-mail address ("name <x@y>" is refused).
func ValidAddress(s string) bool {
	a, err := mail.ParseAddress(s)
	return err == nil && a.Address == s && !strings.ContainsAny(s, "\r\n")
}

// Send delivers one message. to must be a plain address.
func (m *Mailer) Send(to, subject, body string) error {
	if !ValidAddress(to) {
		return errors.New("destinatário inválido")
	}
	if strings.ContainsAny(subject, "\r\n") {
		return errors.New("assunto inválido")
	}
	addr := net.JoinHostPort(m.cfg.Host, strconv.Itoa(m.cfg.Port))
	dialer := &net.Dialer{Timeout: m.cfg.Timeout}
	tlsConfig := &tls.Config{ServerName: m.cfg.Host, MinVersion: tls.VersionTLS12}

	var client *smtp.Client
	if m.cfg.Security == TLS {
		conn, err := tls.DialWithDialer(dialer, "tcp", addr, tlsConfig)
		if err != nil {
			return fmt.Errorf("conectar ao SMTP: %w", err)
		}
		_ = conn.SetDeadline(time.Now().Add(m.cfg.Timeout))
		if client, err = smtp.NewClient(conn, m.cfg.Host); err != nil {
			conn.Close()
			return fmt.Errorf("SMTP: %w", err)
		}
	} else {
		conn, err := dialer.Dial("tcp", addr)
		if err != nil {
			return fmt.Errorf("conectar ao SMTP: %w", err)
		}
		_ = conn.SetDeadline(time.Now().Add(m.cfg.Timeout))
		if client, err = smtp.NewClient(conn, m.cfg.Host); err != nil {
			conn.Close()
			return fmt.Errorf("SMTP: %w", err)
		}
		if m.cfg.Security == StartTLS {
			if ok, _ := client.Extension("STARTTLS"); !ok {
				client.Close()
				return errors.New("SMTP: o servidor não oferece STARTTLS (use --smtp-security=none só para relay local)")
			}
			if err := client.StartTLS(tlsConfig); err != nil {
				client.Close()
				return fmt.Errorf("SMTP STARTTLS: %w", err)
			}
		}
	}
	defer client.Close()

	if m.cfg.Username != "" {
		// PlainAuth itself refuses to send the password over an unencrypted link.
		if err := client.Auth(smtp.PlainAuth("", m.cfg.Username, m.cfg.Password, m.cfg.Host)); err != nil {
			return fmt.Errorf("SMTP autenticação: %w", err)
		}
	}
	if err := client.Mail(m.from.Address); err != nil {
		return fmt.Errorf("SMTP remetente: %w", err)
	}
	if err := client.Rcpt(to); err != nil {
		return fmt.Errorf("SMTP destinatário: %w", err)
	}
	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("SMTP dados: %w", err)
	}
	if _, err := w.Write([]byte(m.message(to, subject, body))); err != nil {
		return fmt.Errorf("SMTP dados: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("SMTP envio: %w", err)
	}
	return client.Quit()
}

func (m *Mailer) message(to, subject, body string) string {
	host := m.from.Address[strings.LastIndex(m.from.Address, "@")+1:]
	body = strings.ReplaceAll(strings.ReplaceAll(body, "\r\n", "\n"), "\n", "\r\n")
	return strings.Join([]string{
		"From: " + m.from.String(),
		"To: " + to,
		"Subject: " + mime.QEncoding.Encode("utf-8", subject),
		"Date: " + time.Now().Format(time.RFC1123Z),
		"Message-ID: <" + strconv.FormatInt(time.Now().UnixNano(), 36) + "@" + host + ">",
		"MIME-Version: 1.0",
		"Content-Type: text/plain; charset=utf-8",
		"Content-Transfer-Encoding: 8bit",
		"", body, "",
	}, "\r\n")
}
