package services

import (
	"context"
	"fmt"
	"time"
)

// NewOutboxService builds an OutboxService over repo (the tbot_queue table)
// and sender (the raw transport). It implements Notifier for the
// orchestrator: every message is persisted before a send is attempted
// (design §8.3), so a crash between "enqueue" and "send" never loses it.
func NewOutboxService(repo OutboxRepository, sender Sender) *OutboxService {
	return &OutboxService{repo: repo, sender: sender}
}

// OutboxService implements Notifier over the reliable-delivery outbox
// pattern (design §8.3): a message is always written to tbot_queue before a
// send is attempted, since in soft mode the reboot begins with no internet
// and the message that matters most ("starting reboot") cannot be delivered
// live.
type OutboxService struct {
	repo   OutboxRepository
	sender Sender
}

var _ Notifier = (*OutboxService)(nil)

// Flush drains every unsent tbot_queue row, oldest first, and attempts
// delivery (design §8.3). A row queued earlier is delivered with its
// original queued time appended (`[queued HH:MM]`, derived from the row's
// created_at, not the flush time) so a human sees when the event actually
// happened. Flush is called at the end of the recovery loop and again at
// the start of the next run. A per-row send failure is recorded on that row
// and does not stop the drain; a per-row failure recording that outcome in
// the store also does not stop the drain (design §8.3: flush must drain
// every unsent row), it is only carried into Flush's return value so the
// caller can log it. Only a failure to list the rows in the first place
// aborts immediately, since there is nothing to iterate over.
func (s *OutboxService) Flush(ctx context.Context) error {
	msgs, err := s.repo.ListUnsentOutboxMessages(ctx)
	if err != nil {
		return fmt.Errorf("outbox: flush: %w", err)
	}

	var lastErr error
	for _, m := range msgs {
		text := m.Text + fmt.Sprintf(" [queued %s]", m.CreatedAt.Format(queuedTimeFormat))
		if err := s.attemptSend(ctx, m.ID, text); err != nil {
			lastErr = fmt.Errorf("outbox: flush message %d: %w", m.ID, err)
		}
	}

	return lastErr
}

// Notify persists text to the outbox before attempting delivery (design
// §8.3): the enqueue always happens first, so the message survives even if
// the send that follows fails or the process crashes mid-send. It returns
// an error only when the outbox write itself, or recording the send
// outcome, fails — a failed live send is the expected path whenever the
// reboot's own uplink is down, and is recorded on the row instead
// (attempts/last_error) for Flush to retry later.
func (s *OutboxService) Notify(ctx context.Context, text string) error {
	id, err := s.repo.EnqueueOutboxMessage(ctx, time.Now().UTC(), text)
	if err != nil {
		return fmt.Errorf("outbox: enqueue message: %w", err)
	}

	return s.attemptSend(ctx, id, text)
}

// queuedTimeFormat renders an outbox row's created_at as "HH:MM" for the
// "[queued HH:MM]" suffix Flush appends to a late message (design §8.3).
const queuedTimeFormat = "15:04"

// attemptSend posts text for the outbox row id and records the outcome:
// sent_at on success, an incremented attempts/last_error on failure. The
// send error itself is captured on the row, not returned — a failed
// delivery is the outbox's normal case (design §8.3); only a failure to
// record that outcome in the store is treated as an error here. sender.Send
// is contractually expected to return an already-redacted error
// (gateway/telegram.Client redacts its own bot token before returning), so
// the raw secret never reaches this layer or the persisted last_error
// column — OutboxService persists whatever it gets back verbatim.
func (s *OutboxService) attemptSend(ctx context.Context, id int64, text string) error {
	sendErr := s.sender.Send(ctx, text)
	if sendErr == nil {
		if err := s.repo.MarkOutboxSent(ctx, id, time.Now().UTC()); err != nil {
			return fmt.Errorf("mark message %d sent: %w", id, err)
		}
		return nil
	}

	if err := s.repo.IncrementOutboxAttempt(ctx, id, sendErr.Error()); err != nil {
		return fmt.Errorf("record failed attempt for message %d: %w", id, err)
	}
	return nil
}
