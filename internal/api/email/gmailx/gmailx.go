// Package gmailx is a minimal Gmail API v1 client (gmail/v1 REST surface)
// covering just what the ported email service needs: profile lookup, watch
// registration, label CRUD, message get, and attachment fetches.
//
// It deliberately avoids google.golang.org/api (a heavy dependency tree) —
// the endpoints used are simple JSON REST calls.
package gmailx

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"golang.org/x/oauth2"
)

const apiBase = "https://gmail.googleapis.com/gmail/v1/users/me"

// Client calls the Gmail API on behalf of one mailbox, refreshing the OAuth2
// access token through the supplied TokenSource.
type Client struct {
	ts     TokenSource
	httpDo *http.Client
}

// TokenSource resolves an OAuth2 token for a link (see DBTokenSource).
type TokenSource interface {
	Token(ctx context.Context) (*oauth2.Token, error)
}

// New builds a client that fetches tokens via ts (already bound to a link).
func New(ts TokenSource) *Client {
	return &Client{ts: ts, httpDo: &http.Client{Timeout: 30 * time.Second}}
}

// apiError is a non-2xx Gmail response.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("gmail api: status %d: %s", e.Status, e.Message)
}

// IsNotFound reports whether err is a Gmail 404.
func IsNotFound(err error) bool {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Status == http.StatusNotFound
	}
	return false
}

// StatusCode returns the HTTP status of an APIError, or 0.
func StatusCode(err error) int {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Status
	}
	return 0
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	tok, err := c.ts.Token(ctx)
	if err != nil {
		return fmt.Errorf("gmail token: %w", err)
	}
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, apiBase+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpDo.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return &APIError{Status: resp.StatusCode, Message: string(msg)}
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Profile is users.getProfile.
type Profile struct {
	EmailAddress  string `json:"emailAddress"`
	MessagesTotal int64  `json:"messagesTotal"`
	ThreadsTotal  int64  `json:"threadsTotal"`
	HistoryID     string `json:"historyId"`
}

func (c *Client) GetProfile(ctx context.Context) (*Profile, error) {
	var p Profile
	if err := c.do(ctx, http.MethodGet, "/profile", nil, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// WatchResponse is users.watch's reply (historyId + expiry).
type WatchResponse struct {
	HistoryID  string `json:"historyId"`
	Expiration string `json:"expiration"` // ms since epoch, as string
}

// Watch registers Pub/Sub push notifications for the mailbox.
func (c *Client) Watch(ctx context.Context, topic string) (*WatchResponse, error) {
	var w WatchResponse
	err := c.do(ctx, http.MethodPost, "/watch", map[string]any{
		"topicName": topic,
		"labelIds":  []string{"INBOX", "SENT", "DRAFT", "STARRED", "IMPORTANT", "TRASH", "SPAM", "UNREAD", "CATEGORY_UPDATES"},
	}, &w)
	if err != nil {
		return nil, err
	}
	return &w, nil
}

// Stop deregisters push notifications.
func (c *Client) Stop(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/stop", map[string]any{}, nil)
}

// Label is a users.labels resource.
type Label struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"` // "system" | "user"
}

func (c *Client) CreateLabel(ctx context.Context, name string) (*Label, error) {
	var l Label
	err := c.do(ctx, http.MethodPost, "/labels", map[string]any{
		"name":                  name,
		"labelListVisibility":   "labelShow",
		"messageListVisibility": "show",
	}, &l)
	if err != nil {
		return nil, err
	}
	return &l, nil
}

func (c *Client) DeleteLabel(ctx context.Context, providerLabelID string) error {
	return c.do(ctx, http.MethodDelete, "/labels/"+providerLabelID, nil, nil)
}

// MessagePartBody carries attachment payload metadata.
type MessagePartBody struct {
	AttachmentID string `json:"attachmentId"`
	Size         int64  `json:"size"`
	Data         string `json:"data"` // base64url when fetching attachment bodies
}

// MessagePart is one MIME part.
type MessagePart struct {
	PartID   string          `json:"partId"`
	MimeType string          `json:"mimeType"`
	Filename string          `json:"filename"`
	Body     MessagePartBody `json:"body"`
	Parts    []MessagePart   `json:"parts"`
}

// Message is users.messages resource (format=full subset).
type Message struct {
	ID           string      `json:"id"`
	ThreadID     string      `json:"threadId"`
	Snippet      string      `json:"snippet"`
	HistoryID    string      `json:"historyId"`
	InternalDate string      `json:"internalDate"`
	LabelIDs     []string    `json:"labelIds"`
	Payload      MessagePart `json:"payload"`
}

func (c *Client) GetMessage(ctx context.Context, providerID string) (*Message, error) {
	var m Message
	if err := c.do(ctx, http.MethodGet, "/messages/"+providerID+"?format=full", nil, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// GetAttachment fetches a users.messages.attachments body (base64url).
func (c *Client) GetAttachment(ctx context.Context, messageID, attachmentID string) ([]byte, error) {
	var b MessagePartBody
	path := fmt.Sprintf("/messages/%s/attachments/%s", messageID, attachmentID)
	if err := c.do(ctx, http.MethodGet, path, nil, &b); err != nil {
		return nil, err
	}
	return base64.URLEncoding.WithPadding(base64.NoPadding).DecodeString(b.Data)
}

// Filter is a users.settings.filters resource.
type Filter struct {
	ID       string `json:"id"`
	Criteria struct {
		From string `json:"from"`
	} `json:"criteria"`
	Action struct {
		AddLabelIDs    []string `json:"addLabelIds"`
		RemoveLabelIDs []string `json:"removeLabelIds"`
	} `json:"action"`
}

type filterList struct {
	Filters []Filter `json:"filter"`
}

// ListBlockedSenders returns sender addresses whose Gmail filter sends mail
// to TRASH (the Rust service's blocked-sender definition).
func (c *Client) ListBlockedSenders(ctx context.Context) ([]string, error) {
	var fl filterList
	if err := c.do(ctx, http.MethodGet, "/settings/filters", nil, &fl); err != nil {
		return nil, err
	}
	var out []string
	for _, f := range fl.Filters {
		for _, l := range f.Action.AddLabelIDs {
			if l == "TRASH" && f.Criteria.From != "" {
				out = append(out, f.Criteria.From)
			}
		}
	}
	return out, nil
}
