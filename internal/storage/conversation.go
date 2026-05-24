package storage

import (
	"context"
	"fmt"
	"time"

	"diesel/internal/chat"
	"diesel/internal/tracing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

// LoadConversation returns the full persisted chat log in insertion order.
func (s *Store) LoadConversation(ctx context.Context) ([]chat.Message, error) {
	ctx, span := tracing.StartSpan(ctx, "storage.conversation.load")
	defer span.End()

	rows, err := s.db.QueryContext(ctx,
		`SELECT role, content, timestamp, emotion, naked FROM messages ORDER BY id`)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("storage: load conversation: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []chat.Message
	for rows.Next() {
		var (
			m   chat.Message
			ts  string
			nak int
		)
		if err := rows.Scan(&m.Role, &m.Content, &ts, &m.Emotion, &nak); err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			return nil, fmt.Errorf("storage: scan message: %w", err)
		}
		// A row written before this field existed, or an unparseable
		// stamp, just yields the zero time — same leniency the JSON
		// loader had with `omitempty` timestamps.
		if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
			m.Timestamp = t
		}
		m.Naked = nak != 0
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("storage: load conversation: %w", err)
	}
	span.SetAttributes(attribute.Int("conversation.messages", len(out)))
	return out, nil
}

// AppendMessages inserts msgs at the end of the conversation log in one
// transaction. Messages are immutable once written, so the hub appends a
// completed user+assistant pair rather than rewriting history.
func (s *Store) AppendMessages(ctx context.Context, msgs ...chat.Message) error {
	if len(msgs) == 0 {
		return nil
	}
	ctx, span := tracing.StartSpan(ctx, "storage.conversation.append",
		attribute.Int("conversation.appended", len(msgs)),
	)
	defer span.End()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("storage: begin: %w", err)
	}
	for _, m := range msgs {
		naked := 0
		if m.Naked {
			naked = 1
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO messages (role, content, timestamp, emotion, naked)
			 VALUES (?, ?, ?, ?, ?)`,
			m.Role, m.Content, m.Timestamp.Format(time.RFC3339Nano), m.Emotion, naked,
		); err != nil {
			_ = tx.Rollback()
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			return fmt.Errorf("storage: append message: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("storage: commit: %w", err)
	}
	return nil
}

// ClearConversation deletes every persisted message — the "New
// Conversation" action.
func (s *Store) ClearConversation(ctx context.Context) error {
	ctx, span := tracing.StartSpan(ctx, "storage.conversation.clear")
	defer span.End()
	if _, err := s.db.ExecContext(ctx, `DELETE FROM messages`); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("storage: clear conversation: %w", err)
	}
	return nil
}
