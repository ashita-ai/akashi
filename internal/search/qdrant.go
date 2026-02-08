package search

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/qdrant/go-client/qdrant"

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
	ID           uuid.UUID
	OrgID        uuid.UUID
	AgentID      string
	DecisionType string
	Confidence   float32
	QualityScore float32
	ValidFrom    time.Time
	Embedding    []float32
}

// QdrantIndex implements Searcher backed by Qdrant Cloud.
type QdrantIndex struct {
	client     *qdrant.Client
	collection string
	dims       uint64
	logger     *slog.Logger

	healthMu  sync.Mutex
	lastCheck time.Time
	lastErr   error
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

// EnsureCollection creates the collection if it doesn't already exist,
// with HNSW parameters tuned for 1024-dim cosine similarity.
func (q *QdrantIndex) EnsureCollection(ctx context.Context) error {
	exists, err := q.client.CollectionExists(ctx, q.collection)
	if err != nil {
		return fmt.Errorf("search: check collection exists: %w", err)
	}
	if exists {
		q.logger.Info("qdrant: collection already exists", "collection", q.collection)
		return nil
	}

	m := uint64(16)
	efConstruct := uint64(128)

	err = q.client.CreateCollection(ctx, &qdrant.CreateCollection{
		CollectionName: q.collection,
		VectorsConfig: qdrant.NewVectorsConfig(&qdrant.VectorParams{
			Size:     q.dims,
			Distance: qdrant.Distance_Cosine,
			HnswConfig: &qdrant.HnswConfigDiff{
				M:           &m,
				EfConstruct: &efConstruct,
			},
		}),
	})
	if err != nil {
		return fmt.Errorf("search: create collection %q: %w", q.collection, err)
	}

	// Create payload indexes for filtered search.
	keywordType := qdrant.FieldType_FieldTypeKeyword
	for _, field := range []string{"org_id", "agent_id", "decision_type"} {
		if _, err := q.client.CreateFieldIndex(ctx, &qdrant.CreateFieldIndexCollection{
			CollectionName: q.collection,
			FieldName:      field,
			FieldType:      &keywordType,
		}); err != nil {
			return fmt.Errorf("search: create index on %q: %w", field, err)
		}
	}

	floatType := qdrant.FieldType_FieldTypeFloat
	for _, field := range []string{"confidence", "quality_score", "valid_from_unix"} {
		if _, err := q.client.CreateFieldIndex(ctx, &qdrant.CreateFieldIndexCollection{
			CollectionName: q.collection,
			FieldName:      field,
			FieldType:      &floatType,
		}); err != nil {
			return fmt.Errorf("search: create index on %q: %w", field, err)
		}
	}

	q.logger.Info("qdrant: created collection with payload indexes", "collection", q.collection, "dims", q.dims)
	return nil
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
			"org_id":          p.OrgID.String(),
			"agent_id":        p.AgentID,
			"decision_type":   p.DecisionType,
			"confidence":      float64(p.Confidence),
			"quality_score":   float64(p.QualityScore),
			"valid_from_unix": float64(p.ValidFrom.Unix()),
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
// to avoid hammering the health endpoint on every search request.
func (q *QdrantIndex) Healthy(ctx context.Context) error {
	q.healthMu.Lock()
	defer q.healthMu.Unlock()

	if time.Since(q.lastCheck) < 5*time.Second {
		return q.lastErr
	}

	_, err := q.client.HealthCheck(ctx)
	q.lastCheck = time.Now()
	if err != nil {
		q.lastErr = fmt.Errorf("search: qdrant unhealthy: %w", err)
	} else {
		q.lastErr = nil
	}
	return q.lastErr
}

// Close shuts down the Qdrant gRPC connection.
func (q *QdrantIndex) Close() error {
	return q.client.Close()
}
