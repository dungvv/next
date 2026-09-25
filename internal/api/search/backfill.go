package search

import (
	"context"
	"errors"
	"log/slog"

	"golang.org/x/sync/errgroup"
)

// BackfillError wraps source/publish/reindex failures (domain::models::
// BackfillError).
type BackfillError struct {
	Op  string // "source" | "publish" | "reindex"
	Err error
}

func (e *BackfillError) Error() string { return e.Op + ": " + e.Err.Error() }
func (e *BackfillError) Unwrap() error { return e.Err }

// PropertyBackfillIndexer directly reindexes one typed entity's denormalized
// properties (outbound::property_search_indexer).
type PropertyBackfillIndexer interface {
	Reindex(ctx context.Context, entityID, entityType string) error
}

// Orchestrator is the Go port of BackfillOrchestrator: drains a source page
// by page, publishing each page's messages (or directly reindexing
// properties), observing cancellation between pages.
type Orchestrator struct {
	Source    *PgBackfillSource
	Publisher SearchEventPublisher
	Indexer   PropertyBackfillIndexer
}

// NewOrchestrator builds the service.
func NewOrchestrator(source *PgBackfillSource, publisher SearchEventPublisher, indexer PropertyBackfillIndexer) *Orchestrator {
	return &Orchestrator{Source: source, Publisher: publisher, Indexer: indexer}
}

// drainSourceWithCursor drives a cursor-paginated source to completion
// (mirrors drain_source_with_cursor).
func drainSourceWithCursor[C any](
	ctx context.Context,
	o *Orchestrator,
	progress *JobProgress,
	fetch func(*C) (SourcePage, *C, error),
) (BackfillReceipt, error) {
	var cursor *C
	enqueued := 0
	for {
		if err := ctx.Err(); err != nil {
			slog.Info("search: backfill cancelled between pages", "enqueued", enqueued)
			return BackfillReceipt{Enqueued: enqueued}, nil
		}
		page, next, err := fetch(cursor)
		if err != nil {
			return BackfillReceipt{}, &BackfillError{Op: "source", Err: err}
		}
		if page.RowsConsumed == 0 {
			break
		}
		if err := o.Publisher.Publish(ctx, page.Messages); err != nil {
			return BackfillReceipt{}, &BackfillError{Op: "publish", Err: err}
		}
		enqueued += page.RowsConsumed
		progress.Add(ctx, page.RowsConsumed)
		// A nil next cursor on a non-empty page means the source exhausted
		// its sort key — stop rather than restart pagination.
		if next == nil {
			break
		}
		cursor = next
	}
	return BackfillReceipt{Enqueued: enqueued}, nil
}

// drainSource drives an offset-paginated source (mirrors drain_source).
func drainSource(
	ctx context.Context,
	o *Orchestrator,
	progress *JobProgress,
	fetch func(offset int) (SourcePage, error),
) (BackfillReceipt, error) {
	offset, enqueued := 0, 0
	for {
		if err := ctx.Err(); err != nil {
			slog.Info("search: backfill cancelled between pages", "enqueued", enqueued)
			return BackfillReceipt{Enqueued: enqueued}, nil
		}
		page, err := fetch(offset)
		if err != nil {
			return BackfillReceipt{}, &BackfillError{Op: "source", Err: err}
		}
		if page.RowsConsumed == 0 {
			break
		}
		if err := o.Publisher.Publish(ctx, page.Messages); err != nil {
			return BackfillReceipt{}, &BackfillError{Op: "publish", Err: err}
		}
		enqueued += page.RowsConsumed
		offset += page.RowsConsumed
		progress.Add(ctx, page.RowsConsumed)
	}
	return BackfillReceipt{Enqueued: enqueued}, nil
}

// drainPropertySource drains typed property pages with bounded-concurrency
// direct reindexes (mirrors drain_property_source, buffer_unordered(20)).
func (o *Orchestrator) drainPropertySource(
	ctx context.Context,
	progress *JobProgress,
	fetch func(offset int) (PropertySourcePage, error),
) (BackfillReceipt, error) {
	if o.Indexer == nil {
		return BackfillReceipt{}, errors.New("search: property indexer not configured")
	}
	offset, enqueued := 0, 0
	for {
		if err := ctx.Err(); err != nil {
			slog.Info("search: property backfill cancelled between pages", "enqueued", enqueued)
			return BackfillReceipt{Enqueued: enqueued}, nil
		}
		page, err := fetch(offset)
		if err != nil {
			return BackfillReceipt{}, &BackfillError{Op: "source", Err: err}
		}
		if page.RowsConsumed == 0 {
			break
		}
		g, gctx := errgroup.WithContext(ctx)
		g.SetLimit(20)
		for _, id := range page.EntityIDs {
			id := id
			g.Go(func() error {
				if err := o.Indexer.Reindex(gctx, id, page.EntityType); err != nil {
					return &BackfillError{Op: "reindex", Err: err}
				}
				return nil
			})
		}
		if err := g.Wait(); err != nil {
			return BackfillReceipt{}, err
		}
		enqueued += page.RowsConsumed
		offset += page.RowsConsumed
		progress.Add(ctx, page.RowsConsumed)
	}
	return BackfillReceipt{Enqueued: enqueued}, nil
}

// Per-entity drains — each runs the job's detached ctx so the drain
// outlives the HTTP request but still observes CancelAllLocal.
func (o *Orchestrator) BackfillDocuments(ctx context.Context, req DocumentBackfillRequest, p *JobProgress) (BackfillReceipt, error) {
	return drainSourceWithCursor(ctx, o, p, func(c *docCursor) (SourcePage, *docCursor, error) {
		return o.Source.FetchDocuments(ctx, req, c)
	})
}

func (o *Orchestrator) BackfillChats(ctx context.Context, req ChatBackfillRequest, p *JobProgress) (BackfillReceipt, error) {
	return drainSourceWithCursor(ctx, o, p, func(c *chatCursor) (SourcePage, *chatCursor, error) {
		return o.Source.FetchChats(ctx, req, c)
	})
}

func (o *Orchestrator) BackfillChannels(ctx context.Context, req ChannelBackfillRequest, p *JobProgress) (BackfillReceipt, error) {
	return drainSource(ctx, o, p, func(offset int) (SourcePage, error) {
		return o.Source.FetchChannels(ctx, req, offset)
	})
}

func (o *Orchestrator) BackfillEmails(ctx context.Context, req EmailBackfillRequest, p *JobProgress) (BackfillReceipt, error) {
	return drainSource(ctx, o, p, func(offset int) (SourcePage, error) {
		return o.Source.FetchEmails(ctx, req, offset)
	})
}

func (o *Orchestrator) BackfillProjects(ctx context.Context, req ProjectBackfillRequest, p *JobProgress) (BackfillReceipt, error) {
	return drainSourceWithCursor(ctx, o, p, func(c *projectCursor) (SourcePage, *projectCursor, error) {
		return o.Source.FetchProjects(ctx, req, c)
	})
}

func (o *Orchestrator) BackfillCalls(ctx context.Context, req CallBackfillRequest, p *JobProgress) (BackfillReceipt, error) {
	return drainSourceWithCursor(ctx, o, p, func(c *callCursor) (SourcePage, *callCursor, error) {
		return o.Source.FetchCalls(ctx, req, c)
	})
}

func (o *Orchestrator) BackfillCalendarEvents(ctx context.Context, req CalendarEventBackfillRequest, p *JobProgress) (BackfillReceipt, error) {
	return drainSourceWithCursor(ctx, o, p, func(c *calendarCursor) (SourcePage, *calendarCursor, error) {
		return o.Source.FetchCalendarEvents(ctx, req, c)
	})
}

func (o *Orchestrator) BackfillEntityProperties(ctx context.Context, req PropertiesBackfillRequest, p *JobProgress) (BackfillReceipt, error) {
	return o.drainPropertySource(ctx, p, func(offset int) (PropertySourcePage, error) {
		return o.Source.FetchEntityProperties(ctx, req, offset)
	})
}
