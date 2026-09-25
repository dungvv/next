package scheduled

import (
	"fmt"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
)

// cronParser accepts the Rust `cron` crate's 6-field format
// (`sec min hour dom mon dow`) plus descriptors like `@daily`.
// `SecondOptional` also tolerates the standard 5-field form.
var cronParser = cron.NewParser(
	cron.SecondOptional | cron.Minute | cron.Hour |
		cron.Dom | cron.Month | cron.Dow | cron.Descriptor,
)

// parseCron parses a cron expression. The Rust `cron` crate additionally
// accepts an optional 7th `year` field; robfig/cron has no year field, so a
// 7-field expression is accepted with the year field dropped — the remaining
// fields already constrain the firing days for every year value in practice.
func parseCron(expr string) (cron.Schedule, error) {
	fields := strings.Fields(expr)
	if len(fields) == 7 {
		expr = strings.Join(fields[:6], " ")
	}
	s, err := cronParser.Parse(expr)
	if err != nil {
		return nil, fmt.Errorf("invalid cron schedule %q: %w", expr, err)
	}
	return s, nil
}

// nextRunAfterNow mirrors Schedule::next_run_after_now: the first firing after
// `now` evaluated in `tz`, returned in UTC.
func nextRunAfterNow(expr, tz string, now time.Time) (time.Time, error) {
	sched, err := parseCron(expr)
	if err != nil {
		return time.Time{}, err
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid timezone %q: %w", tz, err)
	}
	next := sched.Next(now.In(loc))
	if next.IsZero() {
		return time.Time{}, fmt.Errorf("schedule has no future firings")
	}
	return next.UTC(), nil
}
