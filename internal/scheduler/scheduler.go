// Package scheduler replaces EventBridge cron: an embedded cron that publishes
// job envelopes onto the `jobs` JetStream stream, so periodic work flows
// through the same consumer path as everything else.
package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/robfig/cron/v3"

	"github.com/macro-inc/macro/pkg/config"
	"github.com/macro-inc/macro/pkg/events"
	"github.com/macro-inc/macro/pkg/natsx"
)

// periodicJob maps an ex-EventBridge schedule to a jobs-stream subject.
type periodicJob struct {
	name    string // subject suffix: jobs.<name>
	spec    string // cron spec
	payload map[string]any
}

// Ported EventBridge schedules. Each comment cites the authoritative
// scheduleExpression in infra/stacks/ (EventBridge `rate(n units)` becomes
// a cron at the start of each interval).
var periodicJobs = []periodicJob{
	// infra/stacks/deleted-item-poller/lambda.ts — rate(4 hours)
	{"deleted_item_poll", "0 */4 * * *", nil},
	// infra/stacks/organization-retention/organization-retention-trigger.ts — rate(1 day)
	{"org_retention_trigger", "0 0 * * *", nil},
	// infra/stacks/email-service/scheduled_lambda.ts — rate(1 minute)
	{"email_scheduled", "* * * * *", nil},
	// infra/stacks/email-sfs-delete-handler/sfs_delete_lambda.ts — cron(0 8 * * ? *)
	{"email_sfs_delete", "0 8 * * *", nil},
	// infra/stacks/sha-cleanup/index.ts — rate(1 hour)
	{"sha_cleanup", "0 * * * *", nil},
	// infra/stacks/authentication-service/user-link-cleanup-lambda.ts — rate(8 hours)
	{"user_link_cleanup", "0 */8 * * *", nil},
}

// Run registers periodic jobs and blocks until shutdown.
func Run(ctx context.Context, cfg config.Config) error {
	_, js, err := natsx.Connect(ctx, cfg.NatsURL)
	if err != nil {
		return err
	}
	if _, err := natsx.EnsureStream(ctx, js, events.StreamJobs, []string{events.StreamJobs + ".>"}); err != nil {
		return fmt.Errorf("ensure jobs stream: %w", err)
	}

	c := cron.New()
	for _, job := range periodicJobs {
		job := job
		subject := events.StreamJobs + "." + job.name
		if _, err := c.AddFunc(job.spec, func() {
			env, err := events.New("job."+job.name, "scheduler", subject, 1, job.payload)
			if err != nil {
				slog.Error("scheduler: build envelope", "job", job.name, "err", err)
				return
			}
			raw, err := json.Marshal(env)
			if err != nil {
				slog.Error("scheduler: marshal", "job", job.name, "err", err)
				return
			}
			if _, err := js.Publish(ctx, subject, raw); err != nil {
				slog.Error("scheduler: publish", "job", job.name, "err", err)
			}
		}); err != nil {
			return fmt.Errorf("schedule %s: %w", job.name, err)
		}
		slog.Info("scheduler: registered", "job", job.name, "spec", job.spec, "subject", subject)
	}
	c.Start()
	<-ctx.Done()
	c.Stop()
	return nil
}
