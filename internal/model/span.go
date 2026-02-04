package model

import (
	"time"

	"github.com/google/uuid"
)

// SpanKind represents the OTEL span kind.
type SpanKind string

const (
	SpanKindInternal SpanKind = "internal"
	SpanKindClient   SpanKind = "client"
	SpanKindServer   SpanKind = "server"
	SpanKindProducer SpanKind = "producer"
	SpanKindConsumer SpanKind = "consumer"
)

// SpanStatus represents the OTEL span status.
type SpanStatus string

const (
	SpanStatusOK    SpanStatus = "ok"
	SpanStatusError SpanStatus = "error"
	SpanStatusUnset SpanStatus = "unset"
)

// Span is an OTEL-compatible hierarchical trace entry. Immutable.
type Span struct {
	ID           uuid.UUID      `json:"id"`
	RunID        uuid.UUID      `json:"run_id"`
	ParentSpanID *uuid.UUID     `json:"parent_span_id,omitempty"`
	TraceID      *string        `json:"trace_id,omitempty"`
	SpanID       *string        `json:"span_id,omitempty"`
	Name         string         `json:"name"`
	Kind         SpanKind       `json:"kind"`
	StartedAt    time.Time      `json:"started_at"`
	EndedAt      *time.Time     `json:"ended_at,omitempty"`
	Status       SpanStatus     `json:"status"`
	Attributes   map[string]any `json:"attributes"`
	CreatedAt    time.Time      `json:"created_at"`
}
