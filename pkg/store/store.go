// Package store owns the four Postgres connection pools.
// Generated sqlc code lives under pkg/store/<db>db/ (see sqlc.yaml).
package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/macro-inc/macro/pkg/config"
)

// Pools groups the four logical databases that live on one Postgres server.
type Pools struct {
	MacroDB *pgxpool.Pool
	EmailDB *pgxpool.Pool
	CommsDB *pgxpool.Pool
	NotifDB *pgxpool.Pool
}

func Open(ctx context.Context, cfg config.Config) (*Pools, error) {
	open := func(url string) (*pgxpool.Pool, error) {
		p, err := pgxpool.New(ctx, url)
		if err != nil {
			return nil, err
		}
		if err := p.Ping(ctx); err != nil {
			p.Close()
			return nil, err
		}
		return p, nil
	}

	p := &Pools{}
	var err error
	if p.MacroDB, err = open(cfg.MacroDBURL); err != nil {
		return nil, fmt.Errorf("macrodb: %w", err)
	}
	if p.EmailDB, err = open(cfg.EmailDBURL); err != nil {
		p.Close()
		return nil, fmt.Errorf("emaildb: %w", err)
	}
	if p.CommsDB, err = open(cfg.CommsDBURL); err != nil {
		p.Close()
		return nil, fmt.Errorf("commsdb: %w", err)
	}
	if p.NotifDB, err = open(cfg.NotificationDBURL); err != nil {
		p.Close()
		return nil, fmt.Errorf("notifdb: %w", err)
	}
	return p, nil
}

func (p *Pools) Close() {
	for _, pool := range []*pgxpool.Pool{p.MacroDB, p.EmailDB, p.CommsDB, p.NotifDB} {
		if pool != nil {
			pool.Close()
		}
	}
}
