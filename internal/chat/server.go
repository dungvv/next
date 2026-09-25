// HTTP server + routing for the chat service — mirrors the axum routers in
// crates/channels/src/inbound/{axum_router,list_router}.rs mounted under the
// same paths the Rust DSS stack uses (/channels and /comms).
package chat

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/macro-inc/macro/pkg/frecency"
)

// Server is the inbound HTTP adapter for the chat domain.
type Server struct {
	cfg    Config
	db     *store
	fx     *effects
	frec   *frecency.Store // frecency read side; nil without Postgres
	authMW func(http.Handler) http.Handler
}

// NewServer wires handlers onto a chi router.
func NewServer(cfg Config, db *store, fx *effects, frec *frecency.Store, authMW func(http.Handler) http.Handler) *Server {
	return &Server{cfg: cfg, db: db, fx: fx, frec: frec, authMW: authMW}
}

// Handler builds the route tree.
func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(30 * time.Second))
	r.Get("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// All chat routes below require authentication.
	r.Group(func(r chi.Router) {
		r.Use(s.authMW)

		// ---- /channels (crates/channels axum_router) ----
		r.Route("/channels", func(r chi.Router) {
			r.Post("/", s.createChannel)
			r.Post("/get_or_create_dm", s.getOrCreateDM)
			r.Post("/get_or_create_private", s.getOrCreatePrivate)
			r.Post("/mentions", s.createMention)
			r.Delete("/mentions/{mention_id}", s.deleteMention)
			r.Post("/preview", s.channelPreviews)
			r.Post("/join/{join_code}", s.joinChannelByCode)
			r.Get("/activity", s.getActivity)
			r.Post("/activity", s.postActivity)
			r.Get("/attachments/{entity_type}/{entity_id}/references", s.getAttachmentReferences)

			r.Route("/{channel_id}", func(r chi.Router) {
				r.Get("/", s.getChannel)
				r.Patch("/", s.patchChannel)
				r.Delete("/", s.deleteChannel)
				r.Put("/profile_picture", s.setChannelPicture)
				r.Post("/message", s.postMessage)
				r.Get("/messages", s.getChannelMessages)
				r.Get("/messages/catch-up", s.getMessagesCatchUp)
				r.Post("/typing", s.postTyping)
				r.Post("/reaction", s.postReaction)
				r.Patch("/message/{message_id}", s.patchMessage)
				r.Delete("/message/{message_id}", s.deleteMessage)
				r.Get("/messages/{message_id}/replies", s.getThreadReplies)
				r.Get("/messages/{message_id}/context", s.getMessageContext)
				r.Get("/messages/{message_id}/resolve", s.resolveMessage)
				r.Get("/attachments", s.getChannelAttachments)
				r.Get("/participants", s.getParticipants)
				r.Post("/participants", s.addParticipants)
				r.Delete("/participants", s.removeParticipants)
				r.Get("/join-link", s.getJoinLink)
				r.Post("/join", s.joinChannel)
				r.Post("/leave", s.leaveChannel)
			})
		})

		// ---- /comms (list router) ----
		r.Route("/comms", func(r chi.Router) {
			r.Get("/channels", s.listChannels)
			r.Get("/activity", s.commsActivity)
		})
	})
	return r
}
