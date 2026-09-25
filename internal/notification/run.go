// Package notification implements the "macro notification" subcommand:
// JetStream consumers + chi HTTP surface replacing the SQS/SNS/SMTP Rust
// notification service (docs/GO_SELFHOST_PLAN.md).
package notification

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/errgroup"

	"github.com/macro-inc/macro/pkg/config"
	"github.com/macro-inc/macro/pkg/mail"
	"github.com/macro-inc/macro/pkg/natsx"
	"github.com/macro-inc/macro/pkg/push"
)

const serviceName = "notification"

// Run starts the notification service: DB pool, NATS consumers, digest
// flusher, and the HTTP server. Blocks until ctx is cancelled.
func Run(ctx context.Context, cfg config.Config) error {
	ncfg, err := fromRoot(cfg)
	if err != nil {
		return err
	}

	// ---- storage ----
	db, err := pgxpool.New(ctx, cfg.NotificationDBURL)
	if err != nil {
		return fmt.Errorf("notification db: %w", err)
	}
	defer db.Close()
	if err := db.Ping(ctx); err != nil {
		return fmt.Errorf("notification db ping: %w", err)
	}
	repo := NewPGRepository(db)

	// The digest gate's user-existence check (Rust DbUserExistenceChecker)
	// queries the "User" table on macrodb. If a separate macrodb pool can't
	// be established the gate treats every user as existing.
	var userChecker UserChecker
	userDB := db
	if cfg.MacroDBURL != "" && cfg.MacroDBURL != cfg.NotificationDBURL {
		if pool, perr := pgxpool.New(ctx, cfg.MacroDBURL); perr == nil {
			if perr := pool.Ping(ctx); perr == nil {
				userDB = pool
				defer pool.Close()
			} else {
				pool.Close()
				slog.Warn("macrodb ping failed; digest gate will assume users exist", "err", perr)
			}
		} else {
			slog.Warn("macrodb connect failed; digest gate will assume users exist", "err", perr)
		}
	}
	if userDB != nil {
		userChecker = pgUserChecker{db: userDB}
	}

	rdb, err := newRedisClient(cfg)
	if err != nil {
		return err
	}
	defer rdb.Close()
	if err := rdb.Ping(ctx).Err(); err != nil {
		return fmt.Errorf("redis ping: %w", err)
	}

	// ---- messaging ----
	nc, js, err := natsx.Connect(ctx, cfg.NatsURL)
	if err != nil {
		return err
	}
	defer nc.Close()
	if err := EnsureStreams(ctx, js); err != nil {
		return fmt.Errorf("ensure notification stream: %w", err)
	}

	// ---- adapters ----
	mailer, err := mail.NewSMTP(mail.SMTPConfig{
		Host:      cfg.SMTPHost,
		Port:      cfg.SMTPPort,
		User:      cfg.SMTPUser,
		Pass:      cfg.SMTPPass,
		From:      ncfg.MailFrom,
		TLSPolicy: ncfg.SMTPTLSPolicy,
	})
	if err != nil {
		return fmt.Errorf("smtp: %w", err)
	}

	sender := &push.Sender{}
	if ncfg.FCMServiceAccountJSON != "" {
		fcm, err := push.NewFCM(push.FCMConfig{
			ServiceAccountJSON: ncfg.FCMServiceAccountJSON,
			Endpoint:           ncfg.FCMEndpoint,
		})
		if err != nil {
			return fmt.Errorf("fcm: %w", err)
		}
		sender.FCM = fcm
	}
	if ncfg.APNSKeyFile != "" {
		apns, err := push.NewAPNs(push.APNsConfig{
			KeyFile:      ncfg.APNSKeyFile,
			KeyID:        ncfg.APNSKeyID,
			TeamID:       ncfg.APNSTeamID,
			BundleID:     ncfg.APNSBundleID,
			VoIPBundleID: ncfg.APNSVoIPBundleID,
			Sandbox:      ncfg.APNSSandbox,
		})
		if err != nil {
			return fmt.Errorf("apns: %w", err)
		}
		sender.APNs = apns
	}
	if sender.FCM == nil && sender.APNs == nil {
		slog.Warn("no push providers configured; push delivery disabled")
	}

	digests := NewRedisDigestBatcher(rdb)
	gateway := NewGatewayClient(ncfg.GatewayInternalURL, ncfg.InternalAPIKey)
	status := NewStatusPublisher(nc)
	signer := NewURLSigner(ncfg.URLSigningHMAC)

	svc := &Service{
		repo:        repo,
		js:          js,
		nc:          nc,
		status:      status,
		gateway:     gateway,
		pushes:      sender,
		digests:     digests,
		serviceName: serviceName,
		gate: digestGatePorts{
			repo:      repo,
			users:     userChecker,
			online:    redisLastOnline{rdb: rdb},
			digests:   digests,
			window:    ncfg.DigestWindow,
			threshold: ncfg.DigestOnlineThreshold,
		},
	}
	delivery := &Delivery{
		repo:     repo,
		nc:       nc,
		gateway:  gateway,
		pushes:   sender,
		digests:  digests,
		mail:     mailer,
		mailFrom: ncfg.MailFrom,
		// Rust StateMachineDriverB uses a 30m digest window at egress.
		window: ncfg.DigestEgressWindow,
	}
	pushEvents := NewPushEventHandler(repo, digests, ncfg.DigestEgressWindow)
	flusher := NewDigestFlusher(digests, repo, mailer, ncfg.MailFrom,
		ncfg.DigestPollInterval, ncfg.DigestWindow, ncfg.DigestStalenessThreshold,
		signer, ncfg.NotificationServiceURL)

	// ---- background workers ----
	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error { return ingressConsumer(ctx, js, svc) })
	g.Go(func() error { return deliveryConsumer(ctx, js, delivery) })
	g.Go(func() error { return pushEventConsumer(ctx, js, pushEvents) })
	g.Go(func() error {
		flusher.Run(ctx)
		return nil
	})

	// ---- HTTP ----
	router := chi.NewRouter()
	h := Handlers{
		svc:         svc,
		pushEvents:  pushEvents,
		internalKey: ncfg.InternalAPIKey,
		signer:      signer,
		publicURL:   ncfg.NotificationServiceURL,
	}
	h.Register(router)
	// Dual-mount under /{version} (v1, v2) to match the Rust service.
	router.Route("/{version}", h.Register)

	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", ncfg.Port),
		Handler:           router,
		ReadHeaderTimeout: 10 * time.Second,
	}
	g.Go(func() error {
		slog.Info("notification http listening", "port", ncfg.Port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			return err
		}
		return nil
	})
	g.Go(func() error {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	})

	if err := g.Wait(); err != nil && ctx.Err() == nil {
		return err
	}
	return nil
}

// newRedisClient builds a Valkey client. REDIS_URI (e.g. redis://host:6379)
// takes precedence over cfg.ValkeyAddr to match the Rust services' env name.
func newRedisClient(cfg config.Config) (*redis.Client, error) {
	if uri := os.Getenv("REDIS_URI"); uri != "" {
		opt, err := redis.ParseURL(uri)
		if err != nil {
			return nil, fmt.Errorf("parse REDIS_URI: %w", err)
		}
		return redis.NewClient(opt), nil
	}
	return redis.NewClient(&redis.Options{Addr: cfg.ValkeyAddr}), nil
}
