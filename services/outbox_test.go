package services

import (
	"errors"
	"testing"
	"time"
)

func TestOutboxService_Notify(t *testing.T) {
	t.Parallel()

	t.Run("success marks the row sent and delivers the exact text", func(t *testing.T) {
		t.Parallel()

		repo := newFakeOutboxRepo()
		sender := &fakeSender{}
		svc := NewOutboxService(repo, sender)

		text := "initiated (reason: no internet)"
		if err := svc.Notify(t.Context(), text); err != nil {
			t.Fatalf("Notify: %v", err)
		}

		if len(sender.sent) != 1 || sender.sent[0] != text {
			t.Fatalf("sender.sent = %v, want [%q]", sender.sent, text)
		}

		msgs, err := repo.ListUnsentOutboxMessages(t.Context())
		if err != nil {
			t.Fatalf("ListUnsentOutboxMessages: %v", err)
		}
		if len(msgs) != 0 {
			t.Fatalf("unsent messages = %v, want none (row should be marked sent)", msgs)
		}
	})

	t.Run("send failure leaves the row unsent with attempts and last_error recorded", func(t *testing.T) {
		t.Parallel()

		repo := newFakeOutboxRepo()
		sender := &fakeSender{err: errors.New("telegram: send message: status 500")}
		svc := NewOutboxService(repo, sender)

		if err := svc.Notify(t.Context(), "completed, internet restored"); err != nil {
			t.Fatalf("Notify: %v", err)
		}

		msgs, err := repo.ListUnsentOutboxMessages(t.Context())
		if err != nil {
			t.Fatalf("ListUnsentOutboxMessages: %v", err)
		}
		if len(msgs) != 1 {
			t.Fatalf("unsent messages = %d, want 1", len(msgs))
		}
		if msgs[0].Attempts != 1 {
			t.Fatalf("attempts = %d, want 1", msgs[0].Attempts)
		}
		if msgs[0].LastError == "" {
			t.Fatal("last_error is empty, want the send failure recorded")
		}
	})

	t.Run("a sender error already redacted is persisted verbatim", func(t *testing.T) {
		t.Parallel()

		// mirrors the gateway/telegram.Client contract (design R4): Send
		// redacts its own bot token before returning, so OutboxService must
		// persist the error exactly as received — it has no secret of its
		// own to redact and must never need one.
		redactedErr := "telegram: send request: dial tcp: lookup api.telegram.org/bot***/sendMessage: connection refused"
		repo := newFakeOutboxRepo()
		sender := &fakeSender{err: errors.New(redactedErr)}
		svc := NewOutboxService(repo, sender)

		if err := svc.Notify(t.Context(), "completed, internet restored"); err != nil {
			t.Fatalf("Notify: %v", err)
		}

		msgs, err := repo.ListUnsentOutboxMessages(t.Context())
		if err != nil {
			t.Fatalf("ListUnsentOutboxMessages: %v", err)
		}
		if len(msgs) != 1 {
			t.Fatalf("unsent messages = %d, want 1", len(msgs))
		}
		if msgs[0].LastError != redactedErr {
			t.Fatalf("last_error = %q, want the sender's error persisted verbatim %q", msgs[0].LastError, redactedErr)
		}
	})
}

func TestOutboxService_Flush(t *testing.T) {
	t.Parallel()

	t.Run("delivers a queued row with its original queued time", func(t *testing.T) {
		t.Parallel()

		repo := newFakeOutboxRepo()
		sender := &fakeSender{}
		svc := NewOutboxService(repo, sender)

		queuedAt := time.Date(2026, 7, 6, 4, 2, 0, 0, time.UTC)
		if _, err := repo.EnqueueOutboxMessage(t.Context(), queuedAt, "initiated (reason: no internet)"); err != nil {
			t.Fatalf("EnqueueOutboxMessage: %v", err)
		}

		if err := svc.Flush(t.Context()); err != nil {
			t.Fatalf("Flush: %v", err)
		}

		want := "initiated (reason: no internet) [queued 04:02]"
		if len(sender.sent) != 1 || sender.sent[0] != want {
			t.Fatalf("flushed text = %v, want [%q]", sender.sent, want)
		}

		msgs, err := repo.ListUnsentOutboxMessages(t.Context())
		if err != nil {
			t.Fatalf("ListUnsentOutboxMessages: %v", err)
		}
		if len(msgs) != 0 {
			t.Fatalf("unsent messages after flush = %v, want none", msgs)
		}
	})

	t.Run("no unsent rows is a no-op", func(t *testing.T) {
		t.Parallel()

		repo := newFakeOutboxRepo()
		sender := &fakeSender{}
		svc := NewOutboxService(repo, sender)

		if err := svc.Flush(t.Context()); err != nil {
			t.Fatalf("Flush: %v", err)
		}
		if len(sender.sent) != 0 {
			t.Fatalf("sender.sent = %v, want none", sender.sent)
		}
	})

	t.Run("a send failure during flush leaves rows unsent and does not abort the drain", func(t *testing.T) {
		t.Parallel()

		repo := newFakeOutboxRepo()
		sender := &fakeSender{err: errors.New("telegram: send message: status 500")}
		svc := NewOutboxService(repo, sender)

		if _, err := repo.EnqueueOutboxMessage(t.Context(), time.Now().UTC(), "a"); err != nil {
			t.Fatalf("EnqueueOutboxMessage: %v", err)
		}
		if _, err := repo.EnqueueOutboxMessage(t.Context(), time.Now().UTC(), "b"); err != nil {
			t.Fatalf("EnqueueOutboxMessage: %v", err)
		}

		if err := svc.Flush(t.Context()); err != nil {
			t.Fatalf("Flush: %v", err)
		}

		msgs, err := repo.ListUnsentOutboxMessages(t.Context())
		if err != nil {
			t.Fatalf("ListUnsentOutboxMessages: %v", err)
		}
		if len(msgs) != 2 {
			t.Fatalf("unsent messages after failed flush = %d, want 2 (both still queued)", len(msgs))
		}
	})

	t.Run("a store failure recording one row's outcome does not abort the drain", func(t *testing.T) {
		t.Parallel()

		repo := newFakeOutboxRepo()
		sender := &fakeSender{}
		svc := NewOutboxService(repo, sender)

		base := time.Now().UTC()
		failID, err := repo.EnqueueOutboxMessage(t.Context(), base.Add(-time.Minute), "a")
		if err != nil {
			t.Fatalf("EnqueueOutboxMessage: %v", err)
		}
		okID, err := repo.EnqueueOutboxMessage(t.Context(), base, "b")
		if err != nil {
			t.Fatalf("EnqueueOutboxMessage: %v", err)
		}

		// force MarkOutboxSent to fail for failID only (its send succeeds,
		// so Flush reaches the store write and that write is rejected),
		// leaving okID's row to prove the drain continued past it. Replaces
		// the old CREATE TRIGGER white-box against a real *sql.DB.
		repo.markSentErr = map[int64]error{failID: errors.New("forced failure")}

		if err := svc.Flush(t.Context()); err == nil {
			t.Fatal("Flush: want a non-nil aggregate error reporting failID's store failure")
		}

		msgs, err := repo.ListUnsentOutboxMessages(t.Context())
		if err != nil {
			t.Fatalf("ListUnsentOutboxMessages: %v", err)
		}
		if len(msgs) != 1 || msgs[0].ID != failID {
			t.Fatalf("unsent messages after flush = %+v, want only failID=%d (its store write failed; okID=%d must have been attempted)", msgs, failID, okID)
		}
	})
}
