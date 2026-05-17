package main

import (
	"context"
	"encoding/json"
	"os"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

// conversationFile is the on-disk shape of the persisted chat log. It's a
// struct rather than a bare []chatMessage so the format can grow extra
// fields later (timestamps, a title, …) without breaking older files.
type conversationFile struct {
	Messages []chatMessage `json:"messages"`
}

// conversationPath returns the canonical location of the persisted
// conversation, a sibling of settings.json.
func conversationPath() (string, error) {
	return configFilePath("conversation.json")
}

// loadConversation reads the persisted chat log. A missing or unparseable
// file just yields an empty history — a fresh start is always a valid
// outcome, so this never returns an error.
func loadConversation() []chatMessage {
	_, span := startSpan(context.Background(), "conversation.load")
	defer span.End()

	path, err := conversationPath()
	if err != nil {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		// Missing file is the common case on first launch — record it but
		// don't flag the span as an error.
		span.SetAttributes(attribute.Bool("conversation.exists", false))
		return nil
	}
	span.SetAttributes(attribute.Int("conversation.bytes", len(data)))
	var cf conversationFile
	if err := json.Unmarshal(data, &cf); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil
	}
	span.SetAttributes(attribute.Int("conversation.messages", len(cf.Messages)))
	return cf.Messages
}

// saveConversation writes `history` to disk atomically. An empty history
// removes the file entirely so the next launch starts clean rather than
// replaying a blank transcript. `ctx` carries the parent span so the disk
// write nests under the chat.turn it was triggered by; callers outside a
// turn (e.g. the New Conversation menu) pass context.Background().
func saveConversation(ctx context.Context, history []chatMessage) error {
	_, span := startSpan(ctx, "conversation.save",
		attribute.Int("conversation.messages", len(history)),
	)
	defer span.End()

	path, err := conversationPath()
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	if len(history) == 0 {
		span.SetAttributes(attribute.Bool("conversation.removed", true))
		err := os.Remove(path)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		}
		return err
	}
	data, err := json.MarshalIndent(conversationFile{Messages: history}, "", "  ")
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	span.SetAttributes(attribute.Int("conversation.bytes", len(data)))
	if err := atomicWriteFile(path, data, 0o600); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	return nil
}