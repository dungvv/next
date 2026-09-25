// Package mail provides the MailPort interface and an SMTP adapter built on
// wneessen/go-mail. It replaces the Rust SES adapter (EmailServiceOps).
package mail

import (
	"context"
	"fmt"

	gomail "github.com/wneessen/go-mail"
)

// Message is a single outbound email.
type Message struct {
	// To is the recipient address (the email part of a `macro|<email>` user id).
	// May contain a comma-separated address list for multi-recipient sends.
	To string
	// Cc/Bcc are optional comma-separated address lists.
	Cc  string
	Bcc string
	// From overrides the configured default sender when non-empty.
	From string
	// Subject is the email subject line.
	Subject string
	// TextBody is the plain-text body. Required when HTMLBody is empty.
	TextBody string
	// HTMLBody is the HTML body. When both bodies are set the HTML part is
	// added as an alternative to the text part.
	HTMLBody string
}

// Port is the outbound port for sending email (replaces SES).
type Port interface {
	Send(ctx context.Context, msg Message) error
}

// SMTPConfig configures the SMTP adapter.
type SMTPConfig struct {
	Host string
	Port int
	User string
	Pass string
	// From is the default envelope/header sender.
	From string
	// TLSPolicy controls TLS: "mandatory" (default), "opportunistic", or "none".
	TLSPolicy string
}

// SMTP sends mail through a plain SMTP server (Mailhog in dev).
type SMTP struct {
	cfg    SMTPConfig
	policy gomail.TLSPolicy
}

// NewSMTP builds an SMTP adapter.
func NewSMTP(cfg SMTPConfig) (*SMTP, error) {
	if cfg.Host == "" {
		return nil, fmt.Errorf("mail: SMTP host is required")
	}
	policy := gomail.TLSMandatory
	switch cfg.TLSPolicy {
	case "", "mandatory":
		policy = gomail.TLSMandatory
	case "opportunistic":
		policy = gomail.TLSOpportunistic
	case "none":
		policy = gomail.NoTLS
	default:
		return nil, fmt.Errorf("mail: unknown TLS policy %q", cfg.TLSPolicy)
	}
	return &SMTP{cfg: cfg, policy: policy}, nil
}

// Send implements Port.
func (s *SMTP) Send(ctx context.Context, msg Message) error {
	if msg.To == "" {
		return fmt.Errorf("mail: recipient is required")
	}
	if msg.TextBody == "" && msg.HTMLBody == "" {
		return fmt.Errorf("mail: message has no body")
	}

	m := gomail.NewMsg()
	from := msg.From
	if from == "" {
		from = s.cfg.From
	}
	if err := m.From(from); err != nil {
		return fmt.Errorf("mail: bad from address: %w", err)
	}
	if err := m.To(msg.To); err != nil {
		return fmt.Errorf("mail: bad to address: %w", err)
	}
	if msg.Cc != "" {
		if err := m.Cc(msg.Cc); err != nil {
			return fmt.Errorf("mail: bad cc address: %w", err)
		}
	}
	if msg.Bcc != "" {
		if err := m.Bcc(msg.Bcc); err != nil {
			return fmt.Errorf("mail: bad bcc address: %w", err)
		}
	}
	m.Subject(msg.Subject)
	if msg.TextBody != "" {
		m.SetBodyString(gomail.TypeTextPlain, msg.TextBody)
		if msg.HTMLBody != "" {
			m.AddAlternativeString(gomail.TypeTextHTML, msg.HTMLBody)
		}
	} else {
		m.SetBodyString(gomail.TypeTextHTML, msg.HTMLBody)
	}

	opts := []gomail.Option{
		gomail.WithPort(s.cfg.Port),
		gomail.WithTLSPolicy(s.policy),
	}
	if s.cfg.User != "" {
		opts = append(opts,
			gomail.WithSMTPAuth(gomail.SMTPAuthAutoDiscover),
			gomail.WithUsername(s.cfg.User),
			gomail.WithPassword(s.cfg.Pass),
		)
	} else {
		opts = append(opts, gomail.WithSMTPAuth(gomail.SMTPAuthNoAuth))
	}
	client, err := gomail.NewClient(s.cfg.Host, opts...)
	if err != nil {
		return fmt.Errorf("mail: client init: %w", err)
	}
	if err := client.DialAndSendWithContext(ctx, m); err != nil {
		return fmt.Errorf("mail: send to %s: %w", msg.To, err)
	}
	return nil
}
