package search

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/qdrant/go-client/qdrant"
	"golang.org/x/sync/singleflight"

	"github.com/ashita-ai/akashi/internal/model"
)

// QdrantConfig holds configuration for connecting to Qdrant.
type QdrantConfig struct {
	URL        string // e.g. "https://xyz.cloud.qdrant.io:6333" or "http://localhost:6333"
	APIKey     string
	Collection string
	Dims       uint64
}

// Point is the data needed to upsert a single decision into Qdrant.
type Point struct {
	ID                uuid.UUID
	OrgID             uuid.UUID
	AgentID           string
	DecisionType      string
	Confidence        float32
	CompletenessScore float32
	ValidFrom         time.Time
	Embedding         []float32
	SessionID         *uuid.UUID
	Tool              string
	Model             string
	Project           string
}

// QdrantIndex implements Searcher backed by Qdrant Cloud.
type QdrantIndex struct {
	client     *qdrant.Client
	collection string
	dims       uint64
	logger     *slog.Logger

	healthGroup singleflight.Group
	healthErr   atomic.Value // stores *error (pointer-to-error, never nil pointer; inner error may be nil)
	healthAt    atomic.Int64 // unix nanos of last check
}

// parseQdrantURL extracts host, port, and TLS flag from a Qdrant URL.
// Accepts forms like "https://host:6333", "http://host:6333", or "host:6334".
func parseQdrantURL(rawURL string) (host string, port int, useTLS bool, err error) {
	u, parseErr := url.Parse(rawURL)
	if parseErr != nil || u.Host == "" {
		return "", 0, false, fmt.Errorf("search: invalid qdrant URL: %q", rawURL)
	}

	useTLS = u.Scheme == "https"
	host = u.Hostname()

	if portStr := u.Port(); portStr != "" {
		p, err := strconv.Atoi(portStr)
		if err != nil {
			return "", 0, false, fmt.Errorf("search: invalid port in qdrant URL: %q", portStr)
		}
		// If the user specified the REST port (6333), use the gRPC port (6334).
		if p == 6333 {
			port = 6334
		} else {
			port = p
		}
	} else {
		port = 6334
	}

	return host, port, useTLS, nil
}

// NewQdrantIndex creates a new QdrantIndex and connects to the Qdrant server via gRPC.
func NewQdrantIndex(cfg QdrantConfig, logger *slog.Logger) (*QdrantIndex, error) {
	host, port, useTLS, err := parseQdrantURL(cfg.URL)
	if err != nil {
		return nil, err
	}

	client, err := qdrant.NewClient(&qdrant.Config{
		Host:   host,
		Port:   port,
		APIKey: cfg.APIKey,
		UseTLS: useTLS,
	})
	if err != nil {
		return nil, fmt.Errorf("search: connect to qdrant at %s:%d: %w", host, port, err)
	}

	return &QdrantIndex{
		client:     client,
		collection: cfg.Collection,
		dims:       cfg.Dims,
		logger:     logger,
	}, nil
}

// EnsureCollection creates the collection if it doesn't already exist and
// ensures all payload indexes are present. Index creation is always attempted
// regardless of whether the collection pre-existed — CreateFieldIndex is
// idempotent on Qdrant, so this safely backfills any indexes added after the
// collection was first created (e.g. when a field is renamed in a migration).
func (q *QdrantIndex) EnsureCollection(ctx context.Context) error {
	exists, err := q.client.CollectionExists(ctx, q.collection)
	if err != nil {
		return fmt.Errorf("search: check collection exists: %w", err)
	}

	if !exists {
		m := uint64(16)
		efConstruct := uint64(128)

		if err := q.client.CreateCollection(ctx, &qdrant.CreateCollection{
			CollectionName: q.collection,
			VectorsConfig: qdrant.NewVectorsConfig(&qdrant.VectorParams{
				Size:     q.dims,
				Distance: qdrant.Distance_Cosine,
				HnswConfig: &qdrant.HnswConfigDiff{
					M:           &m,
					EfConstruct: &efConstruct,
				},
			}),
		}); err != nil {
			return fmt.Errorf("search: create collection %q: %w", q.collection, err)
		}
		q.logger.Info("qdrant: created collection", "collection", q.collection, "dims", q.dims)
	} else {
		q.logger.Info("qdrant: collection already exists", "collection", q.collection)
	}

	// Always ensure payload indexes exist. CreateFieldIndex is idempotent —
	// calling it on an existing index is a no-op. This guarantees indexes
	// added after the collection was first created are backfilled on restart.
	keywordType := qdrant.FieldType_FieldTypeKeyword
	for _, field := range []string{"org_id", "agent_id", "decision_type", "session_id", "tool", "model", "project"} {
		if _, err := q.client.CreateFieldIndex(ctx, &qdrant.CreateFieldIndexCollection{
			CollectionName: q.collection,
			FieldName:      field,
			FieldType:      &keywordType,
		}); err != nil {
			return fmt.Errorf("search: ensure index on %q: %w", field, err)
		}
	}

	floatType := qdrant.FieldType_FieldTypeFloat
	for _, field := range []string{"confidence", "completeness_score", "valid_from_unix"} {
		if _, err := q.client.CreateFieldIndex(ctx, &qdrant.CreateFieldIndexCollection{
			CollectionName: q.collection,
			FieldName:      field,
			FieldType:      &floatType,
		}); err != nil {
			return fmt.Errorf("search: ensure index on %q: %w", field, err)
		}
	}

	q.logger.Info("qdrant: payload indexes ensured", "collection", q.collection)
	return nil
}

// FindSimilar returns decision IDs with embeddings similar to the given embedding
// within an org. Used internally for conflict detection and consensus scoring.
// excludeID is stripped from results in Go (simpler than a Qdrant filter for one ID).
// project, when non-nil, restricts results to decisions with a matching project payload field.
func (q *QdrantIndex) FindSimilar(ctx context.Context, orgID uuid.UUID, embedding []float32, excludeID uuid.UUID, project *string, limit int) ([]Result, error) {
	if limit <= 0 {
		limit = 50
	}

	must := []*qdrant.Condition{
		qdrant.NewMatch("org_id", orgID.String()),
	}
	if project != nil && *project != "" {
		must = append(must, qdrant.NewMatch("project", *project))
	}

	// Over-fetch by 1 to absorb the excludeID removal.
	fetchLimit := uint64(limit + 1) //nolint:gosec
	scored, err := q.client.Query(ctx, &qdrant.QueryPoints{
		CollectionName: q.collection,
		Query:          qdrant.NewQueryDense(embedding),
		Filter:         &qdrant.Filter{Must: must},
		Limit:          &fetchLimit,
		WithPayload:    qdrant.NewWithPayload(false),
	})
	if err != nil {
		return nil, fmt.Errorf("search: qdrant find similar: %w", err)
	}

	results := make([]Result, 0, len(scored))
	for _, sp := range scored {
		idStr := sp.Id.GetUuid()
		if idStr == "" {
			continue
		}
		decisionID, err := uuid.Parse(idStr)
		if err != nil {
			q.logger.Warn("qdrant: invalid UUID in point ID", "id", idStr)
			continue
		}
		if decisionID == excludeID {
			continue // Strip the source decision from its own neighbor list.
		}
		results = append(results, Result{DecisionID: decisionID, Score: sp.Score})
		if len(results) == limit {
			break
		}
	}

	return results, nil
}

// Search queries Qdrant for decisions matching the embedding and filters.
// org_id is always applied as the first filter (tenant isolation).
// Over-fetches limit*3 to allow re-scoring by the caller.
func (q *QdrantIndex) Search(ctx context.Context, orgID uuid.UUID, embedding []float32, filters model.QueryFilters, limit int) ([]Result, error) {
	must := []*qdrant.Condition{
		qdrant.NewMatch("org_id", orgID.String()),
	}

	if len(filters.AgentIDs) == 1 {
		must = append(must, qdrant.NewMatch("agent_id", filters.AgentIDs[0]))
	} else if len(filters.AgentIDs) > 1 {
		must = append(must, qdrant.NewMatchKeywords("agent_id", filters.AgentIDs...))
	}

	if filters.DecisionType != nil {
		must = append(must, qdrant.NewMatch("decision_type", *filters.DecisionType))
	}

	if filters.ConfidenceMin != nil {
		must = append(must, qdrant.NewRange("confidence", &qdrant.Range{
			Gte: qdrant.PtrOf(float64(*filters.ConfidenceMin)),
		}))
	}

	if filters.TimeRange != nil {
		if filters.TimeRange.From != nil {
			must = append(must, qdrant.NewRange("valid_from_unix", &qdrant.Range{
				Gte: qdrant.PtrOf(float64(filters.TimeRange.From.Unix())),
			}))
		}
		if filters.TimeRange.To != nil {
			must = append(must, qdrant.NewRange("valid_from_unix", &qdrant.Range{
				Lte: qdrant.PtrOf(float64(filters.TimeRange.To.Unix())),
			}))
		}
	}

	if filters.SessionID != nil {
		must = append(must, qdrant.NewMatch("session_id", filters.SessionID.String()))
	}
	if filters.Tool != nil {
		must = append(must, qdrant.NewMatch("tool", *filters.Tool))
	}
	if filters.Model != nil {
		must = append(must, qdrant.NewMatch("model", *filters.Model))
	}
	if filters.Project != nil {
		must = append(must, qdrant.NewMatch("project", *filters.Project))
	}

	fetchLimit := uint64(limit) * 3 //nolint:gosec // limit is bounded by caller (max 1000)
	scored, err := q.client.Query(ctx, &qdrant.QueryPoints{
		CollectionName: q.collection,
		Query:          qdrant.NewQueryDense(embedding),
		Filter:         &qdrant.Filter{Must: must},
		Limit:          &fetchLimit,
		WithPayload:    qdrant.NewWithPayload(false),
	})
	if err != nil {
		return nil, fmt.Errorf("search: qdrant query: %w", err)
	}

	results := make([]Result, 0, len(scored))
	for _, sp := range scored {
		idStr := sp.Id.GetUuid()
		if idStr == "" {
			continue
		}
		decisionID, err := uuid.Parse(idStr)
		if err != nil {
			q.logger.Warn("qdrant: invalid UUID in point ID", "id", idStr)
			continue
		}
		results = append(results, Result{
			DecisionID: decisionID,
			Score:      sp.Score,
		})
	}

	return results, nil
}

// Upsert inserts or updates points in Qdrant.
func (q *QdrantIndex) Upsert(ctx context.Context, points []Point) error {
	if len(points) == 0 {
		return nil
	}

	qdrantPoints := make([]*qdrant.PointStruct, len(points))
	for i, p := range points {
		payload := map[string]any{
			"org_id":             p.OrgID.String(),
			"agent_id":           p.AgentID,
			"decision_type":      p.DecisionType,
			"confidence":         float64(p.Confidence),
			"completeness_score": float64(p.CompletenessScore),
			"valid_from_unix":    float64(p.ValidFrom.Unix()),
		}
		if p.SessionID != nil {
			payload["session_id"] = p.SessionID.String()
		}
		if p.Tool != "" {
			payload["tool"] = p.Tool
		}
		if p.Model != "" {
			payload["model"] = p.Model
		}
		if p.Project != "" {
			payload["project"] = p.Project
		}
		qdrantPoints[i] = &qdrant.PointStruct{
			Id:      qdrant.NewID(p.ID.String()),
			Vectors: qdrant.NewVectorsDense(p.Embedding),
			Payload: qdrant.NewValueMap(payload),
		}
	}

	_, err := q.client.Upsert(ctx, &qdrant.UpsertPoints{
		CollectionName: q.collection,
		Wait:           qdrant.PtrOf(true),
		Points:         qdrantPoints,
	})
	if err != nil {
		return fmt.Errorf("search: qdrant upsert %d points: %w", len(points), err)
	}
	return nil
}

// DeleteByIDs removes specific points from Qdrant by decision ID.
func (q *QdrantIndex) DeleteByIDs(ctx context.Context, ids []uuid.UUID) error {
	if len(ids) == 0 {
		return nil
	}

	pointIDs := make([]*qdrant.PointId, len(ids))
	for i, id := range ids {
		pointIDs[i] = qdrant.NewID(id.String())
	}

	_, err := q.client.Delete(ctx, &qdrant.DeletePoints{
		CollectionName: q.collection,
		Wait:           qdrant.PtrOf(true),
		Points: &qdrant.PointsSelector{
			PointsSelectorOneOf: &qdrant.PointsSelector_Points{
				Points: &qdrant.PointsIdsList{
					Ids: pointIDs,
				},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("search: qdrant delete %d points: %w", len(ids), err)
	}
	return nil
}

// DeleteByOrg removes all points for an organization (GDPR full org deletion).
// Called directly (not via outbox) because the entire org is being wiped — there's
// no need for per-decision outbox entries. The caller is responsible for also deleting
// Postgres data. This method is invoked by the org deletion handler (when implemented).
func (q *QdrantIndex) DeleteByOrg(ctx context.Context, orgID uuid.UUID) error {
	_, err := q.client.Delete(ctx, &qdrant.DeletePoints{
		CollectionName: q.collection,
		Wait:           qdrant.PtrOf(true),
		Points: &qdrant.PointsSelector{
			PointsSelectorOneOf: &qdrant.PointsSelector_Filter{
				Filter: &qdrant.Filter{
					Must: []*qdrant.Condition{
						qdrant.NewMatch("org_id", orgID.String()),
					},
				},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("search: qdrant delete by org %s: %w", orgID, err)
	}
	return nil
}

// Healthy returns nil if Qdrant is reachable. Results are cached for 5 seconds
// to avoid hammering the health endpoint on every search request. Concurrent
// calls after cache expiry are deduplicated via singleflight so only one gRPC
// call is made; all waiters share its result.
func (q *QdrantIndex) Healthy(ctx context.Context) error {
	// Fast path: return the cached result if fresh.
	if time.Since(time.Unix(0, q.healthAt.Load())) < 5*time.Second {
		return q.loadHealthErr()
	}

	// Deduplicate concurrent checks. Use context.Background() instead of the
	// caller's ctx because singleflight reuses the first caller's context —
	// if that caller cancels, all waiters would get a stale error.
	result, _, _ := q.healthGroup.Do("health", func() (any, error) {
		checkCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()

		_, err := q.client.HealthCheck(checkCtx)
		if err != nil {
			wrapped := fmt.Errorf("search: qdrant unhealthy: %w", err)
			q.storeHealthErr(wrapped)
		} else {
			q.storeHealthErr(nil)
		}
		q.healthAt.Store(time.Now().UnixNano())
		return q.loadHealthErr(), nil
	})
	if result == nil {
		return nil
	}
	return result.(error)
}

// storeHealthErr stores an error (or nil) in the atomic.Value.
// atomic.Value cannot store nil directly, so we wrap it in a pointer.
func (q *QdrantIndex) storeHealthErr(err error) {
	q.healthErr.Store(&err)
}

// loadHealthErr loads the cached health error.
func (q *QdrantIndex) loadHealthErr() error {
	v := q.healthErr.Load()
	if v == nil {
		return nil
	}
	return *v.(*error)
}

// Close shuts down the Qdrant gRPC connection.
func (q *QdrantIndex) Close() error {
	return q.client.Close()
}
