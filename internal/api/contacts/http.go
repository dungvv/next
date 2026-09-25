package contacts

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/redis/go-redis/v9"

	"github.com/macro-inc/macro/internal/api/auth"
	"github.com/macro-inc/macro/internal/api/httpx"
)

// addContactRateLimit mirrors PerUserAddContactRateLimit: 50 req/user/hour.
const (
	addContactMaxCount = 50
	addContactWindow   = time.Hour
)

type getContactsResponse struct {
	Contacts []string `json:"contacts"`
}

type addContactRequest struct {
	UserID string `json:"user_id"`
}

// Deps for the contacts router.
type Deps struct {
	Service *Service
	// Redis backs the POST rate limiter; nil disables rate limiting.
	Redis *redis.Client
}

// Register installs GET/POST /contacts on r. Mounted at root and under the
// /contacts gateway prefix by the caller.
func (d Deps) Register(r chi.Router) {
	r.Get("/contacts", d.getContacts)
	r.Post("/contacts", d.rateLimited(d.addContact))
}

func (d Deps) getContacts(w http.ResponseWriter, r *http.Request) {
	caller, ok := auth.FromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	contacts, err := d.Service.QueryContacts(r.Context(), caller.UserID)
	if err != nil {
		slog.Error("contacts: query failed", "err", err)
		httpx.WriteJSON(w, http.StatusInternalServerError, nil)
		return
	}
	if len(contacts) == 0 {
		// QueryContacts always appends self, so this is unreachable in
		// practice, but keep the Rust 404-null contract.
		httpx.WriteJSON(w, http.StatusNotFound, nil)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, getContactsResponse{Contacts: contacts})
}

func (d Deps) addContact(w http.ResponseWriter, r *http.Request) {
	caller, ok := auth.FromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var body addContactRequest
	if !httpx.DecodeJSON(w, r, &body) {
		return
	}
	if err := d.Service.AddContactNodes(r.Context(), []string{caller.UserID, body.UserID}); err != nil {
		slog.Error("contacts: failed to create contact connection", "err", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// rateLimited wraps the POST handler with a fixed-window Redis counter
// (RateLimitServiceImpl + RedisRateLimitAdapter port).
func (d Deps) rateLimited(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d.Redis == nil {
			next(w, r)
			return
		}
		caller, ok := auth.FromContext(r.Context())
		if !ok {
			httpx.Error(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		key := fmt.Sprintf("rate_limit:per-user-add-contact:%s", caller.UserID)
		allowed, err := d.checkRateLimit(r.Context(), key)
		if err != nil {
			slog.Error("contacts: rate limit check failed", "err", err)
			// fail open like the Rust service (errors only logged upstream)
			next(w, r)
			return
		}
		if !allowed {
			httpx.Error(w, http.StatusTooManyRequests, "rate limit exceeded")
			return
		}
		next(w, r)
	}
}

func (d Deps) checkRateLimit(ctx context.Context, key string) (bool, error) {
	n, err := d.Redis.Incr(ctx, key).Result()
	if err != nil {
		return false, err
	}
	if n == 1 {
		if err := d.Redis.Expire(ctx, key, addContactWindow).Err(); err != nil {
			return false, err
		}
	}
	return n <= addContactMaxCount, nil
}
