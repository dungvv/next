// Package api implements the "macro api" subcommand: one chi HTTP server that
// mounts the ported leaf services (static files, image proxy, unfurl,
// contacts, convert) under the same path prefixes their Rust counterparts
// used (docs/GO_SELFHOST_PLAN.md).
package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/redis/go-redis/v9"

	"github.com/macro-inc/macro/internal/api/agentharness"
	"github.com/macro-inc/macro/internal/api/auth"
	"github.com/macro-inc/macro/internal/api/authentication"
	"github.com/macro-inc/macro/internal/api/calendar"
	"github.com/macro-inc/macro/internal/api/contacts"
	"github.com/macro-inc/macro/internal/api/convert"
	"github.com/macro-inc/macro/internal/api/dcs"
	"github.com/macro-inc/macro/internal/api/dss"
	"github.com/macro-inc/macro/internal/api/email"
	"github.com/macro-inc/macro/internal/api/email/gmailx"
	"github.com/macro-inc/macro/internal/api/httpx"
	"github.com/macro-inc/macro/internal/api/imageproxy"
	mcpapi "github.com/macro-inc/macro/internal/api/mcp"
	"github.com/macro-inc/macro/internal/api/mcpauth"
	"github.com/macro-inc/macro/internal/api/scheduled"
	searchapi "github.com/macro-inc/macro/internal/api/search"
	"github.com/macro-inc/macro/internal/api/staticfile"
	"github.com/macro-inc/macro/internal/api/unfurl"
	"github.com/macro-inc/macro/pkg/config"
	"github.com/macro-inc/macro/pkg/identity"
	"github.com/macro-inc/macro/pkg/mail"
	"github.com/macro-inc/macro/pkg/natsx"
	"github.com/macro-inc/macro/pkg/objectstore"
	pkgsearch "github.com/macro-inc/macro/pkg/search"
	"github.com/macro-inc/macro/pkg/store/macrodb"
)

// Run starts the api service: builds the chi router, mounts every ported
// sub-router at its Rust path prefixes, and serves on cfg.Port.
func Run(ctx context.Context, cfg config.Config) error {
	acfg, err := Load()
	if err != nil {
		return err
	}
	acfg.Config = cfg // use the already-loaded root config

	deps, err := buildDeps(ctx, acfg)
	if err != nil {
		return err
	}
	defer deps.close()
	deps.startWorkers(ctx)

	router := deps.router()

	srv := &http.Server{
		Addr:              fmt.Sprintf("0.0.0.0:%d", cfg.Port),
		Handler:           router,
		ReadHeaderTimeout: 10 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		slog.Info("api: listening", "port", cfg.Port, "env", cfg.Env)
		errCh <- srv.ListenAndServe()
	}()
	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
		return nil
	case err := <-errCh:
		return err
	}
}

// serviceDeps bundles every dependency the mounted routers need.
type serviceDeps struct {
	cfg      Config
	pool     *pgxpool.Pool
	objStore objectstore.Store
	nc       *nats.Conn // nil when NATS is unavailable
	js       jetstream.JetStream
	redis    *redis.Client // nil disables the contacts rate limiter

	meta         *staticfile.PgMetadataStore
	contactsSvc  *contacts.Service
	agentHarness *agentharness.Module // nil when its config fails to load
	authn        *authentication.Router

	commsPool *pgxpool.Pool // lazily opened for search (comms_messages)
	emailPool *pgxpool.Pool // lazily opened for search (email_threads) + the email API

	emailDeps *email.Deps // nil when the email DB pool couldn't be built

	scheduledSvc  *scheduled.Service
	scheduler     *scheduled.Dispatcher
	calendarSvc   *calendar.Service
	calendarCfg   calendar.Config
	mcpAuthSvc    *mcpauth.Service
	mcpAuthCfg    mcpauth.Config
	mcpValidator  mcpauth.TokenValidator
	mcpCfg        mcpapi.Config
	mcpOAuthStore mcpauth.InflightAuthStore
	search        *searchapi.Deps
	dcsSvc        *dcs.Service // nil when dcs config/init failed
}

func (d *serviceDeps) close() {
	if d.search != nil && d.search.Jobs != nil {
		d.search.Jobs.CancelAllLocal()
	}
	if d.nc != nil {
		d.nc.Close()
	}
	if d.redis != nil {
		_ = d.redis.Close()
	}
	if d.agentHarness != nil {
		d.agentHarness.Close()
	}
	if d.commsPool != nil {
		d.commsPool.Close()
	}
	if d.emailPool != nil {
		d.emailPool.Close()
	}
	if d.pool != nil {
		d.pool.Close()
	}
}

func buildDeps(ctx context.Context, cfg Config) (*serviceDeps, error) {
	d := &serviceDeps{cfg: cfg}

	// macrodb pool (contacts, static-file metadata, convert backfill).
	pool, err := pgxpool.New(ctx, cfg.MacroDBURL)
	if err != nil {
		return nil, fmt.Errorf("macrodb pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("macrodb ping: %w", err)
	}
	d.pool = pool

	// MinIO / S3.
	objs, err := objectstore.New(objectstore.Config{
		Endpoint:        cfg.S3Endpoint,
		Region:          cfg.S3Region,
		AccessKeyID:     cfg.S3AccessKeyID,
		SecretAccessKey: cfg.S3SecretAccessKey,
		UsePathStyle:    true,
	})
	if err != nil {
		return nil, fmt.Errorf("objectstore: %w", err)
	}
	d.objStore = objs

	// NATS (JetStream) — best effort: without it, convert returns 503, the
	// contacts notifier is a no-op, and queue consumers don't run.
	nc, js, err := natsx.Connect(ctx, cfg.NatsURL)
	if err != nil {
		slog.Warn("api: NATS unavailable; queue-backed features degraded", "err", err)
	} else {
		d.nc, d.js = nc, js
	}

	// Valkey for the contacts rate limiter.
	rc := redis.NewClient(&redis.Options{Addr: cfg.ValkeyAddr})
	if err := rc.Ping(ctx).Err(); err != nil {
		slog.Warn("api: valkey unavailable; contacts rate limit disabled", "err", err)
		_ = rc.Close()
	} else {
		d.redis = rc
	}

	// Static-file metadata store (DynamoDB → macrodb table).
	d.meta = staticfile.NewPgMetadataStore(pool)
	if err := d.meta.EnsureSchema(ctx); err != nil {
		slog.Error("api: static_files schema init failed", "err", err)
	}

	// Contacts service.
	var notifier contacts.Notifier = contacts.NoopNotifier{}
	if d.nc != nil {
		notifier = &contacts.NATSNotifier{NC: d.nc}
	}
	d.contactsSvc = &contacts.Service{
		Repo:     contacts.NewPgRepository(pool),
		Notifier: notifier,
	}

	// Agent harness (agent sessions + sandbox runtime + trigger consumer).
	// Best effort: an absent Docker daemon leaves managed sandboxes disabled,
	// not the whole API down.
	if ahcfg, err := agentharness.Load(); err != nil {
		slog.Warn("api: agentharness config invalid; module disabled", "err", err)
	} else if ah, err := agentharness.New(ctx, ahcfg, pool, d.js); err != nil {
		slog.Warn("api: agentharness init failed; module disabled", "err", err)
	} else {
		d.agentHarness = ah
	}

	// scheduled_action: Postgres store + in-process executor + JetStream
	// wake-ups. The agent TaskRunner stays stubbed until ai_tools lands.
	srepo := scheduled.NewRepo(pool)
	var live scheduled.LiveUpdates = scheduled.NoopLiveUpdates{}
	if d.nc != nil {
		live = scheduled.NATSLiveUpdates{NC: d.nc}
	}
	exec := scheduled.NewInProcessExecutor(srepo, pool, scheduled.UnimplementedTaskRunner{}, live)
	d.scheduledSvc = scheduled.NewService(srepo, exec)
	d.scheduler = scheduled.NewDispatcher(srepo, exec, d.js, d.nc)

	// calendar_service + calendar_events: Postgres repo, stubbed Google
	// provider/token ports (Google Calendar API not yet ported).
	crepo := calendar.NewRepo(pool)
	var calPub calendar.EventPublisher = calendar.NoopPublisher{}
	var calRefresh calendar.RefreshNotifier = calendar.NoopRefreshNotifier{}
	if d.nc != nil {
		calPub = calendar.NATSPublisher{JS: d.js, NC: d.nc}
		calRefresh = calendar.NATSRefreshNotifier{NC: d.nc}
	}
	d.calendarSvc = calendar.NewService(crepo, calendar.StubGoogleProvider{},
		calendar.StubTokenProvider{}, calPub, calRefresh)
	if ccfg, err := calendar.Load(); err != nil {
		slog.Warn("api: calendar config invalid; using defaults", "err", err)
	} else {
		d.calendarCfg = ccfg
	}

	// mcp_auth_proxy: Casdoor OAuth broker; in-flight state in Valkey when
	// available, in-memory otherwise.
	var inflight mcpauth.InflightAuthStore = mcpauth.NewMemoryInflightAuth()
	if d.redis != nil {
		inflight = mcpauth.RedisInflightAuth{Client: d.redis}
	}
	d.mcpOAuthStore = inflight
	if mc, err := mcpauth.Load(); err != nil {
		slog.Warn("api: mcpauth config invalid; router disabled", "err", err)
	} else {
		d.mcpAuthCfg = mc
		var provider mcpauth.OAuthProvider = mcpauth.NullOAuthProvider{}
		if icfg, err := identity.Load(); err == nil {
			if idp, err := identity.NewCasdoor(icfg); err == nil {
				provider = mcpauth.IdentityOAuthProvider{IDP: idp, UpstreamProvider: mc.UpstreamProvider}
				d.mcpValidator = &mcpauth.CasdoorTokenValidator{
					Issuer:   icfg.PublicEndpoint,
					ClientID: icfg.ClientID,
					Lookup:   mcpauth.PgMacroUserLookup{DB: pool},
				}
			} else {
				slog.Warn("api: casdoor provider unavailable for mcpauth", "err", err)
			}
		} else {
			slog.Warn("api: identity config unavailable for mcpauth", "err", err)
		}
		d.mcpAuthSvc = mcpauth.NewService(mc.PublicURL, inflight, provider, mc.AllowedRedirectURIs)
	}
	if mc, err := mcpapi.Load(); err != nil {
		slog.Warn("api: mcp config invalid; using defaults", "err", err)
	} else {
		d.mcpCfg = mc
	}

	// search_processing_service: Postgres FTS index + JetStream consumer.
	if scfg, err := searchapi.Load(); err != nil {
		slog.Warn("api: search config invalid; router disabled", "err", err)
	} else {
		// commsdb/emaildb pools are lazy (no ping) — the channel/email paths
		// fail clearly if the databases are unreachable.
		d.commsPool, _ = pgxpool.New(ctx, cfg.CommsDBURL)
		d.emailPool, _ = pgxpool.New(ctx, cfg.EmailDBURL)
		var opts []pkgsearch.PgOption
		if scfg.VectorDimensions > 0 {
			opts = append(opts, pkgsearch.WithVectorDimensions(scfg.VectorDimensions))
		}
		store := pkgsearch.NewPgStore(pool, opts...)
		if err := store.EnsureSchema(ctx); err != nil {
			slog.Error("api: search_index schema init failed", "err", err)
		}
		jobs := searchapi.NewBackfillJobs(pool,
			time.Duration(scfg.BackfillJobTTLSeconds)*time.Second)
		if err := jobs.EnsureSchema(ctx); err != nil {
			slog.Error("api: search_backfill_jobs schema init failed", "err", err)
		}
		sizes := searchapi.DefaultPageSizes()
		if scfg.BackfillPageSize > 0 {
			sizes = searchapi.PageSizes{
				Documents: scfg.BackfillPageSize, Chats: scfg.BackfillPageSize,
				Channels: scfg.BackfillPageSize, Emails: scfg.BackfillPageSize,
				Projects: scfg.BackfillPageSize, Calls: scfg.BackfillPageSize,
				CalendarEvents: scfg.BackfillPageSize, Properties: scfg.BackfillPageSize,
				EmailBatch: scfg.EmailThreadsPerMessage,
			}
		}
		source := searchapi.NewPgBackfillSource(pool, d.commsPool, d.emailPool, sizes)
		pub := searchapi.NATSPublisher{JS: d.js, NC: d.nc}
		indexer := searchapi.NewIndexer(store, pool, d.commsPool, d.emailPool, nil)
		d.search = &searchapi.Deps{
			Config:       scfg,
			Jobs:         jobs,
			Orchestrator: searchapi.NewOrchestrator(source, pub, searchapi.PgPropertyIndexer{Indexer: indexer}),
			Indexer:      indexer,
			Store:        store,
			Publisher:    pub,
		}
	}

	// email_service: Postgres (emaildb) + MinIO attachments + SMTP send +
	// JetStream jobs. Best-effort: when the email DB can't be opened the
	// /email routes stay unmounted rather than taking the API down.
	if d.emailPool == nil {
		d.emailPool, _ = pgxpool.New(ctx, cfg.EmailDBURL)
	}
	if d.emailPool == nil {
		slog.Warn("api: emaildb pool unavailable; email routes disabled")
	} else {
		d.emailDeps = d.buildEmailDeps(ctx, cfg)
	}

	return d, nil
}

// buildEmailDeps assembles the email_service dependency bundle on top of the
// shared pools: SMTP sender, gmail token store (schema ensured best-effort),
// and the JetStream handles.
func (d *serviceDeps) buildEmailDeps(ctx context.Context, cfg Config) *email.Deps {
	var mailPort mail.Port
	if mp, err := mail.NewSMTP(mail.SMTPConfig{
		Host:      cfg.SMTPHost,
		Port:      cfg.SMTPPort,
		User:      cfg.SMTPUser,
		Pass:      cfg.SMTPPass,
		From:      cfg.MailFrom,
		TLSPolicy: cfg.SMTPTLSPolicy,
	}); err != nil {
		slog.Warn("api: SMTP config invalid; email send disabled", "err", err)
	} else {
		mailPort = mp
	}

	tokens, err := gmailx.NewTokenStore(d.emailPool, cfg.GoogleClientID, cfg.GoogleClientSecret,
		cfg.TokenEncryptionKey)
	if err != nil {
		// A malformed TOKEN_ENCRYPTION_KEY is a config bug — fail the build
		// rather than silently store plaintext.
		slog.Error("api: gmail token store init failed; gmail features disabled", "err", err)
	}
	if tokens != nil {
		if err := tokens.EnsureSchema(ctx); err != nil {
			slog.Warn("api: email_gmail_tokens schema init failed; gmail features degraded", "err", err)
		}
	}

	return &email.Deps{
		Cfg: email.Config{
			AttachmentBucket:   cfg.EmailAttachmentBucket,
			GoogleClientID:     cfg.GoogleClientID,
			GoogleClientSecret: cfg.GoogleClientSecret,
			GmailPubsubTopic:   cfg.GmailPubsubTopic,
			SendUndoDelaySecs:  cfg.EmailSendUndoDelaySecs,
			PresignGetSecs:     cfg.EmailPresignGetSecs,
		},
		Pool:   d.emailPool,
		Store:  d.objStore,
		Mail:   mailPort,
		NC:     d.nc,
		JS:     d.js,
		Tokens: tokens,
	}
}

// authentication builds the authentication_service sub-router from
// pkg/identity config (Casdoor + session JWT signing keys).
func (d *serviceDeps) authentication() (*authentication.Router, error) {
	icfg, err := identity.Load()
	if err != nil {
		return nil, err
	}
	idp, err := identity.NewCasdoor(icfg)
	if err != nil {
		return nil, err
	}
	issuer, err := identity.NewIssuer(icfg)
	if err != nil {
		return nil, err
	}
	validator, err := identity.NewValidator(icfg)
	if err != nil {
		return nil, err
	}
	acfg, err := authentication.Load()
	if err != nil {
		return nil, err
	}
	acfg.Env = d.cfg.Env
	return authentication.New(authentication.Deps{
		Cfg:            acfg,
		Pool:           d.pool,
		Q:              macrodb.New(d.pool),
		Identity:       idp,
		Sessions:       issuer,
		Validator:      validator,
		Redis:          d.redis,
		Admin:          idp,
		APITokenKey:    icfg.MacroAPITokenPrivateKey,
		APITokenIssuer: icfg.MacroAPITokenIssuer,
		APITokenTTL:    icfg.MacroAPITokenTTL,
	})
}

// startWorkers launches the background loops the Rust services ran
// in-process: the contacts ingress consumer + backfill outbox poller and the
// S3 (MinIO) upload-event subscriber.
func (d *serviceDeps) startWorkers(ctx context.Context) {
	if d.nc != nil && d.meta != nil {
		staticfile.SubscribeUploadEvents(ctx, d.nc, d.cfg.StaticFileEventsTopic, d.meta)
	}
	if d.js != nil && d.contactsSvc != nil {
		go func() {
			if err := contacts.RunConsumer(ctx, d.js, d.contactsSvc); err != nil &&
				!errors.Is(err, context.Canceled) {
				slog.Error("contacts: consumer exited", "err", err)
			}
		}()
	}
	if d.contactsSvc != nil {
		go d.contactsSvc.RunOutboxWorker(ctx)
	}
	if d.agentHarness != nil {
		d.agentHarness.StartWorkers(ctx, d.js)
	}
	// scheduled_action dispatcher: 30s Postgres ticker + jobs.scheduled_due
	// JetStream consumer.
	if d.scheduler != nil {
		go func() {
			if err := d.scheduler.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
				slog.Error("scheduled: dispatcher exited", "err", err)
			}
		}()
	}
	// search_processing consumer: search.index durable (replaces the Kafka
	// consumer + SQS workers).
	if d.js != nil && d.search != nil {
		go func() {
			if err := searchapi.RunConsumer(ctx, d.js, d.search.Indexer); err != nil &&
				!errors.Is(err, context.Canceled) {
				slog.Error("search: consumer exited", "err", err)
			}
		}()
	}
}

func (d *serviceDeps) router() chi.Router {
	cfg := d.cfg
	r := chi.NewRouter()
	r.Use(middleware.RequestID, middleware.RealIP, middleware.Recoverer,
		corsMiddleware(corsAllowedOrigins()))

	authMw := auth.Middleware(cfg.InternalAPIKey)
	// internalMw gates the service-to-service /internal surface: only the
	// shared internal key authenticates (Rust InternalOnly).
	internalMw := auth.InternalMiddleware(cfg.InternalAPIKey)

	// authentication_service — built up front so its /internal endpoints can
	// merge into the shared /internal mount below. Best-effort: incomplete
	// Casdoor/JWT env leaves the routes unmounted rather than failing the
	// whole api.
	if authn, err := d.authentication(); err != nil {
		slog.Warn("api: authentication router disabled", "err", err)
	} else {
		d.authn = authn
	}

	// NOTE on ordering: chi's Mount (used by Route) shadows handlers already
	// registered on the mounted node itself. So every prefixed sub-router is
	// mounted FIRST, then the same services' root-level routes are registered
	// on r afterwards (mirroring the Rust dual mount_at_root_and_prefix).

	// --- static_file_service ---------------------------------------------
	// Rust mounts file::router() under /api (with health) and /internal.
	meta := d.meta
	sf := staticfile.Deps{
		Metadata:   meta,
		Store:      d.objStore,
		Bucket:     cfg.StaticStorageBucket,
		ServiceURL: cfg.StaticFileServiceURL,
	}
	r.Route("/api", func(r chi.Router) {
		r.Get("/health", healthText)
		r.Group(func(r chi.Router) {
			r.Use(authMw)
			sf.Register(r)
		})
	})

	// --- convert_service ---------------------------------------------------
	cv := convert.Deps{
		JS:        d.js,
		Store:     d.objStore,
		DB:        convert.NewPgDocxQuerier(d.pool),
		DocBucket: cfg.DocumentStorageBucket,
	}
	if d.js != nil {
		if err := convert.EnsureStream(context.Background(), d.js); err != nil {
			slog.Error("api: ensure jobs stream", "err", err)
		}
	}
	r.Route("/internal", func(r chi.Router) {
		r.Use(internalMw)
		sf.Register(r) // static files: same router for internal callers
		cv.Register(r)
		if d.authn != nil {
			// authentication_service internal endpoints — gated by their
			// own x-internal-auth-key check inside.
			d.authn.RegisterInternal(r)
		}
		if d.search != nil {
			// search_processing_service internal endpoints (Rust mounted
			// them at root too): /internal/delete/{document_id},
			// /internal/extract_sync, /internal/backfill/*.
			d.search.RegisterInternal(r)
		}
		if d.emailDeps != nil {
			// email_service internal endpoints: /internal/messages/*,
			// /internal/backfill/provider/gmail*, /internal/delete_user/*.
			d.emailDeps.RegisterInternal(r)
		}
	})
	r.Route("/convert", func(r chi.Router) {
		r.Get("/health", healthJSON)
		r.Route("/internal", func(r chi.Router) {
			r.Use(internalMw)
			cv.Register(r)
		})
	})

	// --- image_proxy_service ----------------------------------------------
	// Rust mounts the inner router at root and under /image-proxy.
	ip := imageproxy.NewDeps()
	imageproxyRoutes := func(r chi.Router) {
		r.Group(func(r chi.Router) {
			r.Use(authMw)
			ip.Register(r)
		})
	}
	r.Route("/image-proxy", func(r chi.Router) {
		r.Get("/health", healthJSON)
		imageproxyRoutes(r)
	})

	// --- unfurl_service ----------------------------------------------------
	// Rust mounts api_router (/unfurl GET+bulk, /proxy GET) plus /health at
	// root and under /unfurl. The root /proxy slot belongs to the image proxy
	// in this combined binary (see report); unfurl's proxy stays reachable at
	// /unfurl/proxy.
	uf := unfurl.NewDeps()
	r.Route("/unfurl", func(r chi.Router) {
		uf.Register(r, true)
		r.Get("/health", healthText)
	})

	// --- contacts_service --------------------------------------------------
	cdeps := contacts.Deps{Service: d.contactsSvc, Redis: d.redis}
	contactsRoutes := func(r chi.Router) {
		r.Group(func(r chi.Router) {
			r.Use(authMw)
			cdeps.Register(r)
		})
	}
	r.Route("/contacts", func(r chi.Router) {
		r.Get("/health", healthText)
		contactsRoutes(r)
	})

	// --- authentication_service ------------------------------------------
	// Rust dual-mounts the auth router at root and under /auth (the gateway
	// prefix); the root half is registered below, after all Route() calls.
	if d.authn != nil {
		r.Route("/auth", func(r chi.Router) {
			r.Get("/health", healthText)
			d.authn.Register(r)
		})
	}

	// --- agent_harness_service --------------------------------------------
	// Rust mounts the inner router at root and under /agent-harness, with
	// the runtime gateway nested at /runtime. /runtime/ws authenticates
	// harness tokens itself, so it stays outside the user-auth group.
	if d.agentHarness != nil {
		ah := d.agentHarness
		agentRoutes := func(r chi.Router) {
			r.Group(func(r chi.Router) {
				r.Use(authMw)
				ah.Register(r)
			})
		}
		r.Route("/agent-harness", func(r chi.Router) {
			r.Get("/health", healthText)
			ah.RegisterRuntime(r)
			agentRoutes(r)
		})
		ah.RegisterRuntime(r)
		agentRoutes(r)
	}

	// --- document_storage_service -----------------------------------------
	// Rust mounts the same router at root, under /{version}, and under /dss.
	// The combined binary mounts only under /dss (Service.Register adds the
	// /{version} branch itself) to avoid root-path collisions with the other
	// mounted services.
	dssSvc := dss.NewService(context.Background(), dss.Deps{
		Pool:                        d.pool,
		Store:                       d.objStore,
		DocBucket:                   cfg.DocumentStorageBucket,
		NC:                          d.nc,
		JS:                          d.js,
		DocumentPermissionJWTSecret: cfg.DocumentPermissionJWT,
	})
	r.Route("/dss", func(r chi.Router) {
		r.Group(func(r chi.Router) {
			r.Use(authMw)
			dssSvc.Register(r)
		})
	})

	// --- scheduled_action -------------------------------------------------
	// Rust dual-mounts at root and under /scheduled-action; the root half is
	// registered below with the other root-level mounts.
	if d.scheduledSvc != nil {
		r.Route("/scheduled-action", func(r chi.Router) {
			r.Get("/health", healthJSON)
			r.Group(func(r chi.Router) {
				r.Use(authMw)
				scheduled.Deps{Service: d.scheduledSvc}.RegisterActions(r)
			})
		})
	}

	// --- calendar_service -------------------------------------------------
	// Rust mounts watch webhook + mutation router at root and under
	// /calendar; the occurrence reads (calendar_events crate) lived in DSS at
	// root — mounted at root here too.
	if d.calendarSvc != nil {
		calDeps := calendar.Deps{Service: d.calendarSvc, Config: d.calendarCfg}
		r.Route("/calendar", func(r chi.Router) {
			r.Get("/health", healthText)
			calDeps.RegisterPublic(r)
			r.Group(func(r chi.Router) {
				r.Use(authMw)
				calDeps.RegisterPrivate(r)
			})
		})
	}

	// --- mcp_service + mcp_auth_proxy --------------------------------------
	// Rust mounts the OAuth broker + bearer-protected /mcp at root and under
	// /mcp. Root half is registered below (without /health).
	if d.mcpAuthSvc != nil || d.mcpValidator != nil {
		r.Route("/mcp", func(r chi.Router) {
			if d.mcpAuthSvc != nil {
				mcpauth.Deps{Service: d.mcpAuthSvc, Config: d.mcpAuthCfg}.Register(r)
			}
			if d.mcpValidator != nil {
				mcpapi.Deps{
					Catalog:   mcpapi.StaticCatalog{},
					Caller:    mcpapi.NotImplementedCaller{},
					Validator: d.mcpValidator,
					Config:    d.mcpCfg,
				}.Register(r)
			}
		})
	}

	// --- search_processing_service ----------------------------------------
	// Rust mounts the internal router at root and under /search-processing;
	// the internal half merges into the shared /internal mount below, and the
	// FTS query endpoint registers at root.
	if d.search != nil {
		r.Route("/search-processing", func(r chi.Router) {
			r.Get("/health", healthText)
			r.Route("/internal", func(r chi.Router) {
				r.Use(internalMw)
				d.search.RegisterInternal(r)
			})
			r.Group(func(r chi.Router) {
				r.Use(authMw)
				d.search.RegisterQuery(r)
			})
		})
	}

	// --- email_service -----------------------------------------------------
	// Rust mounts the email API under /email behind auth, the gmail Pub/Sub
	// webhook at /gmail/webhook (no user auth), and internal endpoints under
	// /internal (merged into the shared mount above).
	if d.emailDeps != nil {
		r.Route("/email", func(r chi.Router) {
			r.Get("/health", healthText)
			r.Group(func(r chi.Router) {
				r.Use(authMw)
				d.emailDeps.Register(r)
			})
		})
		// Pub/Sub can't send our auth headers; the webhook verifies the
		// Google OIDC bearer token itself (see email/webhook.go) and unknown
		// mailboxes are acked to stop retries.
		r.Route("/gmail", func(r chi.Router) {
			d.emailDeps.RegisterGmailWebhook(r)
		})
	}

	// --- document_cognition_service ----------------------------------------
	// Rust mounts the DCS router at root and under /cognition. The combined
	// binary mounts only under /cognition: the root mount would collide with
	// /preview (dss + agent_harness) and /attachments/{id} (email). The DCS
	// handlers authenticate per-route (OptionalMiddleware supplies the caller
	// when credentials are present; anonymous access is allowed for public
	// link-shared chats, citations, and previews, mirroring the Rust
	// OptionalMacroAuthorizationExtractor / access-level extractors).
	if dcsCfg, err := dcs.Load(); err != nil {
		slog.Warn("api: dcs config invalid; cognition routes disabled", "err", err)
	} else if svc, err := dcs.New(dcs.Deps{Pool: d.pool, JS: d.js, NC: d.nc, Cfg: dcsCfg}); err != nil {
		slog.Warn("api: dcs init failed; cognition routes disabled", "err", err)
	} else {
		d.dcsSvc = svc
		if d.js != nil {
			if err := dcs.EnsureStreams(context.Background(), d.js); err != nil {
				slog.Error("api: ensure chats event stream", "err", err)
			}
		}
		r.Route("/cognition", func(r chi.Router) {
			r.Get("/health", healthJSON)
			r.Group(func(r chi.Router) {
				r.Use(auth.OptionalMiddleware(cfg.InternalAPIKey))
				svc.Register(r)
			})
		})
	}

	// --- root-level mounts (after all Route() calls) -----------------------
	imageproxyRoutes(r)
	uf.Register(r, false)
	contactsRoutes(r)
	if d.authn != nil {
		// Root half of the Rust dual mount; /health and /internal stay
		// owned by the combined service above (auth internal endpoints were
		// merged into the shared /internal mount).
		d.authn.RegisterNoInternal(r)
	}
	if d.scheduledSvc != nil {
		// scheduled_action root half: /scheduled-actions*.
		r.Group(func(r chi.Router) {
			r.Use(authMw)
			scheduled.Deps{Service: d.scheduledSvc}.RegisterActions(r)
		})
	}
	if d.calendarSvc != nil {
		// calendar root half: POST /notifications (token-verified, no user
		// auth) plus the /calendar-events + /events + /calendars reads and
		// mutations the Rust binary served at root / via DSS.
		calDeps := calendar.Deps{Service: d.calendarSvc, Config: d.calendarCfg}
		calDeps.RegisterPublic(r)
		r.Group(func(r chi.Router) {
			r.Use(authMw)
			calDeps.RegisterPrivate(r)
		})
	}
	if d.mcpAuthSvc != nil {
		// mcp_auth_proxy root half: /.well-known/*, /authorize, /register,
		// /oauth/callback, /token (the /mcp service endpoint is only under
		// the /mcp mount above).
		mcpauth.Deps{Service: d.mcpAuthSvc, Config: d.mcpAuthCfg}.RegisterOAuth(r)
	}
	if d.search != nil {
		// search root half: POST /search query endpoint. The /internal
		// half merges into the shared /internal mount inside router()
		// below — see the search comment there.
		r.Group(func(r chi.Router) {
			r.Use(authMw)
			d.search.RegisterQuery(r)
		})
	}

	// --- shared ------------------------------------------------------------
	// One top-level health for the combined service (each Rust service had
	// its own; prefixed variants above keep per-service probes).
	r.Get("/health", healthText)

	return r
}

// healthText mirrors the "healthy" plain-text handler used by
// static_file_service, unfurl_service, contacts_service.
func healthText(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	_, _ = w.Write([]byte("healthy"))
}

// healthJSON mirrors the EmptyResponse health handlers in
// image_proxy_service / convert_service (JSON `{}`).
func healthJSON(w http.ResponseWriter, _ *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, httpx.EmptyResponse{})
}

// defaultCORSOrigins is the CORS_ALLOWED_ORIGINS fallback: the local web dev
// servers.
const defaultCORSOrigins = "http://localhost:3000,http://localhost:5173"

// corsAllowedOrigins parses CORS_ALLOWED_ORIGINS (comma-separated) into a
// lookup set; empty entries are dropped.
func corsAllowedOrigins() map[string]struct{} {
	raw := os.Getenv("CORS_ALLOWED_ORIGINS")
	if raw == "" {
		raw = defaultCORSOrigins
	}
	allowed := map[string]struct{}{}
	for _, o := range strings.Split(raw, ",") {
		if o = strings.TrimSpace(o); o != "" {
			allowed[o] = struct{}{}
		}
	}
	return allowed
}

// corsMiddleware replaces macro_cors::cors_layer for the self-host build:
// the Origin is reflected only when it appears in the CORS_ALLOWED_ORIGINS
// allowlist, and credentials are allowed only for those origins.
func corsMiddleware(allowed map[string]struct{}) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if _, ok := allowed[origin]; origin != "" && ok {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Access-Control-Allow-Credentials", "true")
				w.Header().Add("Vary", "Origin")
				w.Header().Set("Access-Control-Allow-Headers",
					"Authorization, Content-Type, "+auth.HeaderInternalAPIKey+", "+auth.HeaderUserID)
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			}
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
