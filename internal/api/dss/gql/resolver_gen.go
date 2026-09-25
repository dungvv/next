package gql

// Resolver wiring for the complete-graph schema.
//
// THIS FILE IS gqlgen-GENERATED SHAPE but hand-maintained: gqlgen was run
// once against static_assets/schema.graphql to produce generated.go,
// models_gen.go and the resolver skeletons; the skeletons are implemented
// here (mostly as explicit "not implemented" errors so unsupported
// operations surface cleanly instead of panicking).
//
// Implemented resolvers:
//   - SoupQueryRoot.user        → viewer (id = authenticated macro user id)
//   - GraphqlUser.soup          → Backend.SoupPage
//
// Everything else returns errNotImplemented. TODO(port): favorites,
// activity, email threads/links, grouped soup, all mutations and
// subscriptions.

import (
	"context"
	"errors"
	"fmt"
)

// Resolver carries the Backend port the implemented resolvers delegate to.
type Resolver struct {
	Backend Backend
}

// errNotImplemented is the resolver-level "unsupported" error.
func errNotImplemented(field string) error {
	return fmt.Errorf("%s is not implemented in the Go port yet", field)
}

// backend resolves the Backend or errors when unwired.
func (r *Resolver) backend() (Backend, error) {
	if r.Backend == nil {
		return nil, errors.New("gql backend not configured")
	}
	return r.Backend, nil
}

// ---------- mutations ----------

func (r *completeMutationRootResolver) SetEntityProperty(ctx context.Context, input SetEntityPropertyInput) (*GraphqlProperty, error) {
	return nil, errNotImplemented("setEntityProperty")
}

func (r *completeMutationRootResolver) UpdateEntityPropertyOptions(ctx context.Context, input UpdateEntityPropertyOptionsInput) ([]GraphqlProperty, error) {
	return nil, errNotImplemented("updateEntityPropertyOptions")
}

func (r *completeMutationRootResolver) RenameEntities(ctx context.Context, inputs []RenameEntityInput) (*EntityMutationPayload, error) {
	return nil, errNotImplemented("renameEntities")
}

func (r *completeMutationRootResolver) MoveEntities(ctx context.Context, inputs []MoveEntityInput) (*EntityMutationPayload, error) {
	return nil, errNotImplemented("moveEntities")
}

func (r *completeMutationRootResolver) UpdateEntitySharePolicies(ctx context.Context, inputs []UpdateEntitySharePolicyInput) (*EntityMutationPayload, error) {
	return nil, errNotImplemented("updateEntitySharePolicies")
}

func (r *completeMutationRootResolver) TrashEntities(ctx context.Context, entities []EntityRefInput) (*EntityMutationPayload, error) {
	return nil, errNotImplemented("trashEntities")
}

func (r *completeMutationRootResolver) RestoreEntities(ctx context.Context, entities []EntityRefInput) (*EntityMutationPayload, error) {
	return nil, errNotImplemented("restoreEntities")
}

func (r *completeMutationRootResolver) DeleteEntitiesPermanently(ctx context.Context, entities []EntityRefInput) (*EntityMutationPayload, error) {
	return nil, errNotImplemented("deleteEntitiesPermanently")
}

func (r *completeMutationRootResolver) DuplicateEntities(ctx context.Context, inputs []DuplicateEntityInput) (*EntityMutationPayload, error) {
	return nil, errNotImplemented("duplicateEntities")
}

func (r *completeMutationRootResolver) SetEntityFavorite(ctx context.Context, entity EntityRefInput, favorite bool) (GraphqlEntityMutationResult, error) {
	return nil, errNotImplemented("setEntityFavorite")
}

func (r *completeMutationRootResolver) SetFavorite(ctx context.Context, entity EntityRefInput, favorite bool) (*SetFavoritePayload, error) {
	return nil, errNotImplemented("setFavorite")
}

func (r *completeMutationRootResolver) ReorderFavorites(ctx context.Context, input ReorderFavoritesInput) ([]GraphqlFavorite, error) {
	return nil, errNotImplemented("reorderFavorites")
}

func (r *completeMutationRootResolver) RecordChannelActivity(ctx context.Context, input RecordChannelActivityInput) (*GraphqlChannelActivity, error) {
	return nil, errNotImplemented("recordChannelActivity")
}

func (r *completeMutationRootResolver) UpdateNotifications(ctx context.Context, input UpdateNotificationsInput) ([]GraphqlNotification, error) {
	return nil, errNotImplemented("updateNotifications")
}

func (r *completeMutationRootResolver) UpdateNotificationsForEntity(ctx context.Context, input UpdateNotificationsForEntityInput) ([]GraphqlNotification, error) {
	return nil, errNotImplemented("updateNotificationsForEntity")
}

func (r *completeMutationRootResolver) MarkEmailThreadSeen(ctx context.Context, input MarkEmailThreadSeenInput) (*GraphqlSoupEmailThread, error) {
	return nil, errNotImplemented("markEmailThreadSeen")
}

func (r *completeMutationRootResolver) MarkEmailThreadUnread(ctx context.Context, input MarkEmailThreadUnreadInput) (*GraphqlSoupEmailThread, error) {
	return nil, errNotImplemented("markEmailThreadUnread")
}

func (r *completeMutationRootResolver) UpdateEmailThreadLabel(ctx context.Context, input UpdateEmailThreadLabelInput) (*GraphqlSoupEmailThread, error) {
	return nil, errNotImplemented("updateEmailThreadLabel")
}

func (r *completeMutationRootResolver) SaveEmailDraft(ctx context.Context, input SaveEmailDraftInput) (*SaveEmailDraftPayload, error) {
	return nil, errNotImplemented("saveEmailDraft")
}

func (r *completeMutationRootResolver) DeleteEmailDraft(ctx context.Context, input DeleteEmailDraftInput) (*DeleteEmailDraftPayload, error) {
	return nil, errNotImplemented("deleteEmailDraft")
}

// ---------- subscriptions ----------

// Subscriptions are not implemented: they require the soup_realtime
// pipeline. TODO(port): bridge NATS subjects to these channels.
func (r *completeSubscriptionRootResolver) SoupUpdates(ctx context.Context) (<-chan []SoupPatch, error) {
	return nil, errNotImplemented("soupUpdates")
}

func (r *completeSubscriptionRootResolver) NotificationUpdates(ctx context.Context) (<-chan GraphqlNotificationPatch, error) {
	return nil, errNotImplemented("notificationUpdates")
}

func (r *completeSubscriptionRootResolver) ActivityUpdates(ctx context.Context) (<-chan GraphqlActivityPatch, error) {
	return nil, errNotImplemented("activityUpdates")
}

// ---------- GraphqlUser fields ----------

func (r *graphqlUserResolver) Favorites(ctx context.Context, obj *GraphqlUser, filter *FavoritesFilterInput) ([]GraphqlFavorite, error) {
	// Favorites are stored in the Pin table in the Rust schema; the soup
	// favorites model is a deeper port. Return an empty list rather than an
	// error so feeds render. TODO(port): map Pin rows to GraphqlFavorite.
	return []GraphqlFavorite{}, nil
}

func (r *graphqlUserResolver) Activity(ctx context.Context, obj *GraphqlUser, input ActivityFeedInput) (*GraphqlActivityPage, error) {
	return nil, errNotImplemented("user.activity")
}

func (r *graphqlUserResolver) ActivityOverview(ctx context.Context, obj *GraphqlUser, input ActivityOverviewInput) (*GraphqlActivityOverview, error) {
	return nil, errNotImplemented("user.activityOverview")
}

func (r *graphqlUserResolver) EmailThread(ctx context.Context, obj *GraphqlUser, input EmailThreadInput) (*GraphqlSoupEmailThread, error) {
	return nil, errNotImplemented("user.emailThread")
}

func (r *graphqlUserResolver) GroupSoup(ctx context.Context, obj *GraphqlUser, input GroupedSoupInput) (*GroupedSoup, error) {
	return nil, errNotImplemented("user.groupSoup")
}

func (r *graphqlUserResolver) Soup(ctx context.Context, obj *GraphqlUser, input SoupInput) (*SoupPage, error) {
	b, err := r.backend()
	if err != nil {
		return nil, err
	}
	page, err := b.SoupPage(ctx, input)
	if err != nil {
		return nil, err
	}
	return &SoupPage{Items: page.Items, NextCursor: page.NextCursor}, nil
}

// ---------- query root ----------

func (r *soupQueryRootResolver) User(ctx context.Context) (*GraphqlUser, error) {
	b, err := r.backend()
	if err != nil {
		return nil, err
	}
	uid := b.ViewerUserID(ctx)
	if uid == "" {
		return nil, errors.New("unauthenticated")
	}
	// emailLabels / emailLinks are non-resolver struct fields — leave them
	// as empty lists (email service not ported).
	return &GraphqlUser{
		ID:          uid,
		EmailLabels: []GraphqlSoupEmailLabel{},
		EmailLinks:  []GraphqlEmailLink{},
	}, nil
}

// ---------- resolver roots ----------

func (r *Resolver) CompleteMutationRoot() CompleteMutationRootResolver {
	return &completeMutationRootResolver{r}
}

func (r *Resolver) CompleteSubscriptionRoot() CompleteSubscriptionRootResolver {
	return &completeSubscriptionRootResolver{r}
}

func (r *Resolver) GraphqlUser() GraphqlUserResolver { return &graphqlUserResolver{r} }

func (r *Resolver) SoupQueryRoot() SoupQueryRootResolver { return &soupQueryRootResolver{r} }

type completeMutationRootResolver struct{ *Resolver }
type completeSubscriptionRootResolver struct{ *Resolver }
type graphqlUserResolver struct{ *Resolver }
type soupQueryRootResolver struct{ *Resolver }
