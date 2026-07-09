package domain

import "time"

// OutboxMessage is one row of the tg_outbox table, as returned by
// OutboxRepository.ListUnsentOutboxMessages.
type OutboxMessage struct {
	ID        int64
	CreatedAt time.Time
	Text      string
	Attempts  int
	LastError string
}
