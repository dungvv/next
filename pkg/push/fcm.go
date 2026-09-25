package push

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"golang.org/x/oauth2/google"
)

// fcmScope is the OAuth2 scope required by FCM HTTP v1.
const fcmScope = "https://www.googleapis.com/auth/firebase.messaging"

// FCMConfig configures the FCM HTTP v1 adapter.
type FCMConfig struct {
	// ServiceAccountJSON is the path to a GCP service-account key file.
	// The file provides both the OAuth2 credentials and the project ID.
	ServiceAccountJSON string
	// Endpoint is the FCM base URL (override for tests).
	Endpoint string
	// HTTPClient is optional; defaults to a client authenticated with the
	// service-account token source.
	HTTPClient *http.Client
}

// FCM sends pushes through the FCM HTTP v1 API using a service account.
type FCM struct {
	projectID string
	endpoint  string
	client    *http.Client
}

// NewFCM builds an FCM adapter from a service-account JSON key file.
func NewFCM(cfg FCMConfig) (*FCM, error) {
	if cfg.ServiceAccountJSON == "" {
		return nil, fmt.Errorf("push: FCM service account JSON path is required")
	}
	raw, err := os.ReadFile(cfg.ServiceAccountJSON)
	if err != nil {
		return nil, fmt.Errorf("push: read FCM service account: %w", err)
	}
	var meta struct {
		ProjectID string `json:"project_id"`
	}
	if err := json.Unmarshal(raw, &meta); err != nil {
		return nil, fmt.Errorf("push: parse FCM service account: %w", err)
	}
	if meta.ProjectID == "" {
		return nil, fmt.Errorf("push: FCM service account JSON missing project_id")
	}

	client := cfg.HTTPClient
	if client == nil {
		jwtCfg, err := google.JWTConfigFromJSON(raw, fcmScope)
		if err != nil {
			return nil, fmt.Errorf("push: FCM JWT config: %w", err)
		}
		client = &http.Client{
			Transport: &oauth2Transport{src: jwtCfg.TokenSource(context.Background())},
			Timeout:   15 * time.Second,
		}
	}

	endpoint := cfg.Endpoint
	if endpoint == "" {
		endpoint = "https://fcm.googleapis.com"
	}
	return &FCM{projectID: meta.ProjectID, endpoint: endpoint, client: client}, nil
}

// fcmMessage is the FCM HTTP v1 message envelope.
type fcmMessage struct {
	Message fcmMessageInner `json:"message"`
}

type fcmMessageInner struct {
	Token        string            `json:"token"`
	Notification *fcmNotification  `json:"notification,omitempty"`
	Data         map[string]string `json:"data,omitempty"`
	Android      *fcmAndroid       `json:"android,omitempty"`
	Apns         *fcmApns          `json:"apns,omitempty"`
}

type fcmNotification struct {
	Title string `json:"title,omitempty"`
	Body  string `json:"body,omitempty"`
}

type fcmAndroid struct {
	CollapseKey  string                  `json:"collapse_key,omitempty"`
	Priority     string                  `json:"priority,omitempty"` // "NORMAL" | "HIGH"
	Notification *fcmAndroidNotification `json:"notification,omitempty"`
}

type fcmAndroidNotification struct {
	Title string `json:"title,omitempty"`
	Body  string `json:"body,omitempty"`
}

type fcmApns struct {
	Headers map[string]string `json:"headers,omitempty"`
}

// fcmError mirrors the FCM v1 error response shape.
type fcmError struct {
	Error struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
		Details []struct {
			Type      string `json:"@type"`
			ErrorCode string `json:"errorCode"`
		} `json:"details"`
	} `json:"error"`
}

// Send implements Port.
func (f *FCM) Send(ctx context.Context, msg Message) (string, error) {
	data := map[string]string{}
	for k, v := range msg.Data {
		if s, ok := v.(string); ok {
			data[k] = s
		} else if v != nil {
			raw, err := json.Marshal(v)
			if err == nil {
				data[k] = string(raw)
			}
		}
	}

	body := fcmMessage{Message: fcmMessageInner{
		Token: msg.Token,
		Data:  data,
	}}
	if msg.PushType == PushTypeBackground {
		// Data-only (silent) message: no visible notification block.
		body.Message.Android = &fcmAndroid{CollapseKey: msg.CollapseKey, Priority: "NORMAL"}
	} else {
		body.Message.Notification = &fcmNotification{Title: msg.Title, Body: msg.Body}
		body.Message.Android = &fcmAndroid{
			CollapseKey:  msg.CollapseKey,
			Priority:     "HIGH",
			Notification: &fcmAndroidNotification{Title: msg.Title, Body: msg.Body},
		}
	}
	if msg.CollapseKey != "" {
		body.Message.Apns = &fcmApns{Headers: map[string]string{"apns-collapse-id": msg.CollapseKey}}
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("push: marshal FCM message: %w", err)
	}

	url := fmt.Sprintf("%s/v1/projects/%s/messages:send", f.endpoint, f.projectID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := f.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("push: FCM request: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("push: read FCM response: %w", err)
	}

	if resp.StatusCode == http.StatusOK {
		var ok struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(respBody, &ok)
		return ok.Name, nil
	}

	var fcmErr fcmError
	if err := json.Unmarshal(respBody, &fcmErr); err == nil && fcmErr.Error.Status != "" {
		for _, d := range fcmErr.Error.Details {
			if d.ErrorCode == "UNREGISTERED" {
				return "", fmt.Errorf("%w: %s", ErrTokenUnregistered, fcmErr.Error.Message)
			}
		}
		// NOT_FOUND from FCM v1 means the token is unknown to the project.
		if fcmErr.Error.Status == "NOT_FOUND" {
			return "", fmt.Errorf("%w: %s", ErrTokenUnregistered, fcmErr.Error.Message)
		}
		return "", fmt.Errorf("push: FCM send failed (%s): %s", fcmErr.Error.Status, fcmErr.Error.Message)
	}
	return "", fmt.Errorf("push: FCM send failed with status %d: %s", resp.StatusCode, string(respBody))
}
