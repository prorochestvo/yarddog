package services

import (
	"context"
	"time"

	"github.com/prorochestvo/yarddog/domain"
)

// Checker probes connectivity (design §4, §5). infrastructure.Checker is the
// production implementation.
type Checker interface {
	Check(ctx context.Context) domain.Result
	Gateway(ctx context.Context) domain.TargetResult
}

// Rebooter requests a router reboot (design §5). gateway/router's device
// drivers, built through its factory, are the production implementations.
type Rebooter interface {
	Reboot(ctx context.Context) error
}

// Credentialer proves the daemon can still authenticate against the router
// web UI without rebooting it: CheckCredentials logs in and immediately
// attempts a best-effort logout (cookie-jar reset), returning nil when the
// login succeeds. Kept separate from Rebooter so the read-only daemon holds
// only this interface, making it structurally incapable of rebooting — the
// two capabilities are different verbs on the same device. Defined here (in
// services, not in gateway/router) because services is the only layer both
// the driver implementation and the credential probe consumer can import
// without violating the inward-only dependency rule. gateway/router's device
// drivers are the production implementations; the credential health probe
// (gateway/router/credential_probe.go) is the sole consumer.
type Credentialer interface {
	CheckCredentials(ctx context.Context) error
}

// Notifier sends a message through the outbox (design §8.3). OutboxService is
// the production implementation; Notify enqueues before it attempts a live
// send and swallows a live-send failure, so a Telegram outage on its own is
// never a reason to fail a run.
type Notifier interface {
	Notify(ctx context.Context, text string) error
	Flush(ctx context.Context) error
}

// Clock abstracts both "now" and "wait one interval" so the recovery loop
// tests without a real sleep. infrastructure.SystemClock is the production
// implementation.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

// RunRepository is the run/check persistence the orchestrator consumes.
// infrastructure.Store satisfies it. InsertRun's returned id and UpdateRun's
// id parameter are opaque UUIDv7 strings (issue #4) that the orchestrator
// only ever threads through, never inspects.
type RunRepository interface {
	InsertRun(ctx context.Context, startedAt time.Time, mode string, internetOK *bool) (string, error)
	UpdateRun(ctx context.Context, id string, u domain.RunUpdate) error
	InsertCheck(ctx context.Context, c domain.Check) error
	GetLastRebootStartedAt(ctx context.Context) (time.Time, bool, error)
}

// MetricsCollector takes one host telemetry snapshot (design
// plans/003-host-telemetry.md). It never returns an error: an unsupported,
// absent, or failed collector is represented as an unavailable
// domain.MetricSample instead of a Collect failure. infrastructure's
// build-tagged linux/darwin collectors are the production implementations.
type MetricsCollector interface {
	Collect(ctx context.Context) domain.HostMetrics
}

// MetricsRepository is the metrics/host persistence the orchestrator
// consumes. infrastructure.Store satisfies it. runID is the opaque UUIDv7
// string InsertRun returned (issue #4).
type MetricsRepository interface {
	SaveMetrics(ctx context.Context, runID string, ts time.Time, m domain.HostMetrics) error
}

// PingCollector takes one round of ICMP ping probes (issue #2). It never
// returns an error: an unreachable host, DNS failure, or missing ping binary
// is represented as an unavailable domain.PingResult instead of a Collect
// failure, mirroring MetricsCollector's contract. infrastructure's
// build-tagged linux/darwin collectors are the production implementations.
type PingCollector interface {
	Collect(ctx context.Context) []domain.PingResult
}

// PingRepository is the ping persistence the orchestrator consumes.
// infrastructure.Store satisfies it. runID is the opaque UUIDv7 string
// InsertRun returned (issue #4).
type PingRepository interface {
	SavePings(ctx context.Context, runID string, ts time.Time, results []domain.PingResult) error
}

// OutboxRepository is the tbot_queue persistence OutboxService consumes.
// infrastructure.Store satisfies it.
type OutboxRepository interface {
	EnqueueOutboxMessage(ctx context.Context, createdAt time.Time, text string) (int64, error)
	ListUnsentOutboxMessages(ctx context.Context) ([]domain.OutboxMessage, error)
	MarkOutboxSent(ctx context.Context, id int64, sentAt time.Time) error
	IncrementOutboxAttempt(ctx context.Context, id int64, sendErr string) error
}

// Sender delivers one message to the transport. gateway/telegram.Client is
// the implementation; it is responsible for redacting its own secrets from
// returned errors before OutboxService ever sees them.
type Sender interface {
	Send(ctx context.Context, text string) error
}

// Settings is the narrow slice of Config the orchestrator needs (design §6,
// §7, §8.4). The composition root destructures the whole Config into this at
// wiring time, so services never needs to import infrastructure's Config type.
// RebootEnabled and MetricsEnabled are bare orchestrator toggles rather than
// domain types (plans/003-host-telemetry.md Trade-off T1): domain owns the
// vocabulary the toggles drive (ActionSkippedDisabled, OutcomeRebootDisabled,
// the Collector consts), not the toggle itself.
type Settings struct {
	Label            string
	RebootCooldown   time.Duration
	RecoveryInterval time.Duration
	RecoveryTimeout  time.Duration
	RebootEnabled    bool
	MetricsEnabled   bool
	PingEnabled      bool
}
