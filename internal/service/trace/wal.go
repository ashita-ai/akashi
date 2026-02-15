// Package trace provides the event ingestion pipeline with buffered COPY-based writes.
// This file implements a write-ahead log (WAL) for crash-durable event buffering.
//
// Architecture:
//
//	Client → Append() → WAL (disk) → in-memory buffer → flush() → COPY to Postgres → WAL cleanup
//	                     ↑ durable                        ↑ durable
//
// The WAL sits between the HTTP handler and the in-memory buffer. Every event is
// written to disk before the handler returns. After a successful COPY flush, the
// WAL checkpoint advances and old segment files are reclaimed.
//
// On startup, recovery reads un-flushed WAL records and populates the in-memory
// buffer before the HTTP server starts accepting traffic.
package trace

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/metric"

	"github.com/ashita-ai/akashi/internal/model"
	"github.com/ashita-ai/akashi/internal/telemetry"
)

// WAL segment file format constants.
const (
	walMagic      = 0x414B5745 // "AKWE" — Akashi WAL Event
	walVersion    = 1
	walHeaderSize = 16 // magic(4) + version(2) + reserved(2) + baseLSN(8)
	walRecordHead = 12 // lsn(8) + payloadLen(4)
	walCRCSize    = 4
	walMaxPayload = 16 << 20 // 16 MB per record

	defaultSegmentSize    = 64 << 20 // 64 MB
	defaultSegmentRecords = 100_000
	minSegmentSize        = 1 << 20 // 1 MB
	minSegmentRecords     = 100

	defaultSyncInterval = 10 * time.Millisecond
)

var crc32cTable = crc32.MakeTable(crc32.Castagnoli)

// WALConfig holds configuration for the write-ahead log.
type WALConfig struct {
	Dir            string        // Directory for WAL files. Empty = WAL disabled.
	SyncMode       string        // "full", "batch", "none". Default: "batch".
	SyncInterval   time.Duration // Sync interval for batch mode. Default: 10ms.
	MaxSegmentSize int64         // Bytes before segment rotation. Default: 64 MB.
	MaxSegmentRecs int           // Records before segment rotation. Default: 100K.
}

// WAL provides crash-durable event buffering via a write-ahead log.
type WAL struct {
	dir      string
	syncMode string

	mu          sync.Mutex // guards segment writes
	current     *os.File   // current open segment
	segmentNum  uint64     // current segment number
	segmentSize int64      // bytes written to current segment
	segmentRecs int        // records written to current segment
	nextLSN     atomic.Uint64

	maxSegSize int64
	maxSegRecs int

	logger *slog.Logger

	// Batch sync goroutine.
	syncCancel context.CancelFunc
	syncDone   chan struct{}
}

// checkpoint tracks the last flushed position.
type checkpoint struct {
	FlushedLSN uint64    `json:"flushed_lsn"`
	FlushedAt  time.Time `json:"flushed_at"`
	Segment    uint64    `json:"segment"`
}

// NewWAL creates a new WAL. Returns nil if cfg.Dir is empty (WAL disabled).
func NewWAL(logger *slog.Logger, cfg WALConfig) (*WAL, error) {
	if cfg.Dir == "" {
		return nil, nil
	}

	if cfg.SyncMode == "" {
		cfg.SyncMode = "batch"
	}
	switch cfg.SyncMode {
	case "full", "batch", "none":
	default:
		return nil, fmt.Errorf("wal: invalid sync mode %q (must be full, batch, or none)", cfg.SyncMode)
	}
	if cfg.SyncInterval <= 0 {
		cfg.SyncInterval = defaultSyncInterval
	}
	if cfg.MaxSegmentSize <= 0 {
		cfg.MaxSegmentSize = defaultSegmentSize
	}
	if cfg.MaxSegmentSize < minSegmentSize {
		return nil, fmt.Errorf("wal: segment size %d too small (min %d)", cfg.MaxSegmentSize, minSegmentSize)
	}
	if cfg.MaxSegmentRecs <= 0 {
		cfg.MaxSegmentRecs = defaultSegmentRecords
	}
	if cfg.MaxSegmentRecs < minSegmentRecords {
		return nil, fmt.Errorf("wal: segment records %d too small (min %d)", cfg.MaxSegmentRecs, minSegmentRecords)
	}

	if err := os.MkdirAll(cfg.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("wal: create directory: %w", err)
	}

	// Verify directory is writable.
	probe := filepath.Join(cfg.Dir, ".wal_probe")
	f, err := os.Create(probe) //nolint:gosec // path is constructed from validated config
	if err != nil {
		return nil, fmt.Errorf("wal: directory not writable: %w", err)
	}
	_ = f.Close()
	_ = os.Remove(probe)

	w := &WAL{
		dir:        cfg.Dir,
		syncMode:   cfg.SyncMode,
		maxSegSize: cfg.MaxSegmentSize,
		maxSegRecs: cfg.MaxSegmentRecs,
		logger:     logger,
	}

	// Load checkpoint to determine next LSN and segment number.
	cp, err := w.loadCheckpoint()
	if err != nil {
		return nil, fmt.Errorf("wal: load checkpoint: %w", err)
	}
	w.nextLSN.Store(cp.FlushedLSN + 1)

	// Find the highest existing segment number.
	highSeg, err := w.highestSegment()
	if err != nil {
		return nil, fmt.Errorf("wal: scan segments: %w", err)
	}
	if highSeg > 0 {
		w.segmentNum = highSeg + 1
	} else {
		w.segmentNum = cp.Segment + 1
	}

	// Open a new segment for writing.
	if err := w.rotateSegment(); err != nil {
		return nil, fmt.Errorf("wal: open initial segment: %w", err)
	}

	if cfg.SyncMode == "none" {
		logger.Warn("wal: sync mode is 'none'; events may be lost on crash (use 'batch' or 'full' in production)")
	}

	if cfg.SyncMode == "batch" {
		ctx, cancel := context.WithCancel(context.Background())
		w.syncCancel = cancel
		w.syncDone = make(chan struct{})
		go w.syncLoop(ctx, cfg.SyncInterval)
	}

	w.registerMetrics()
	return w, nil
}

// Write appends events to the WAL. In "full" sync mode, the segment is synced
// before returning. In "batch" or "none" mode, writes go to the OS page cache.
func (w *WAL) Write(events []model.AgentEvent) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	for i := range events {
		payload, err := json.Marshal(&events[i])
		if err != nil {
			return fmt.Errorf("wal: marshal event: %w", err)
		}
		if len(payload) > walMaxPayload {
			return fmt.Errorf("wal: event payload too large (%d bytes, max %d)", len(payload), walMaxPayload)
		}

		lsn := w.nextLSN.Add(1) - 1

		// Write record: [LSN(8) | payloadLen(4) | payload(N) | CRC32C(4)]
		var head [walRecordHead]byte
		binary.BigEndian.PutUint64(head[0:8], lsn)
		binary.BigEndian.PutUint32(head[8:12], uint32(len(payload))) //nolint:gosec // bounded by walMaxPayload check above

		h := crc32.New(crc32cTable)
		_, _ = h.Write(head[:])
		_, _ = h.Write(payload)
		crc := h.Sum32()

		var crcBuf [walCRCSize]byte
		binary.BigEndian.PutUint32(crcBuf[:], crc)

		if _, err := w.current.Write(head[:]); err != nil {
			return fmt.Errorf("wal: write record head: %w", err)
		}
		if _, err := w.current.Write(payload); err != nil {
			return fmt.Errorf("wal: write payload: %w", err)
		}
		if _, err := w.current.Write(crcBuf[:]); err != nil {
			return fmt.Errorf("wal: write crc: %w", err)
		}

		recordSize := int64(walRecordHead + len(payload) + walCRCSize)
		w.segmentSize += recordSize
		w.segmentRecs++

		// Rotate if needed.
		if w.segmentSize >= w.maxSegSize || w.segmentRecs >= w.maxSegRecs {
			if err := w.rotateSegment(); err != nil {
				return fmt.Errorf("wal: rotate segment: %w", err)
			}
		}
	}

	if w.syncMode == "full" {
		if err := w.current.Sync(); err != nil {
			return fmt.Errorf("wal: fsync: %w", err)
		}
	}

	return nil
}

// Checkpoint advances the flushed position and deletes old segments.
// Call after a successful COPY flush to Postgres.
func (w *WAL) Checkpoint(flushedEvents []model.AgentEvent) error {
	if len(flushedEvents) == 0 {
		return nil
	}

	// Determine the highest LSN among flushed events by scanning WAL records.
	// Since we don't store LSN on AgentEvent, we track by count: the checkpoint
	// advances by len(flushedEvents) records past the last checkpoint.
	cp, err := w.loadCheckpoint()
	if err != nil {
		return fmt.Errorf("wal: load checkpoint for advance: %w", err)
	}

	newLSN := cp.FlushedLSN + uint64(len(flushedEvents))
	newCP := checkpoint{
		FlushedLSN: newLSN,
		FlushedAt:  time.Now().UTC(),
		Segment:    w.segmentNum,
	}

	if err := w.saveCheckpoint(newCP); err != nil {
		return err
	}

	// Delete segments whose records are all below the flushed LSN.
	return w.cleanupSegments(newLSN)
}

// Recover reads un-flushed events from WAL files. Returns events that were
// written to the WAL but not yet confirmed flushed to Postgres.
func (w *WAL) Recover() ([]model.AgentEvent, error) {
	cp, err := w.loadCheckpoint()
	if err != nil {
		return nil, fmt.Errorf("wal: load checkpoint for recovery: %w", err)
	}

	segments, err := w.listSegments()
	if err != nil {
		return nil, fmt.Errorf("wal: list segments for recovery: %w", err)
	}

	var recovered []model.AgentEvent
	for _, seg := range segments {
		events, highLSN, err := w.readSegment(seg)
		if err != nil {
			w.logger.Warn("wal: recovery: error reading segment, skipping remainder",
				"segment", seg, "error", err, "recovered_so_far", len(recovered))
			break
		}
		_ = highLSN
		for _, e := range events {
			if e.lsn > cp.FlushedLSN {
				recovered = append(recovered, e.event)
			}
		}
	}

	return recovered, nil
}

// Close syncs and closes the current segment file. Stops the batch sync goroutine.
func (w *WAL) Close() error {
	if w.syncCancel != nil {
		w.syncCancel()
		<-w.syncDone
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	if w.current != nil {
		if err := w.current.Sync(); err != nil {
			w.logger.Warn("wal: final sync failed", "error", err)
		}
		return w.current.Close()
	}
	return nil
}

// PendingBytes returns the approximate bytes in un-flushed WAL segments.
func (w *WAL) PendingBytes() int64 {
	cp, err := w.loadCheckpoint()
	if err != nil {
		return 0
	}

	segments, err := w.listSegments()
	if err != nil {
		return 0
	}

	var total int64
	for _, seg := range segments {
		info, err := os.Stat(seg)
		if err != nil {
			continue
		}
		total += info.Size()
	}
	// Subtract estimated flushed portion (rough: assume uniform distribution).
	_ = cp
	return total
}

// SegmentCount returns the number of WAL segment files.
func (w *WAL) SegmentCount() int {
	segs, _ := w.listSegments()
	return len(segs)
}

// --- Internal methods ---

type walRecord struct {
	lsn   uint64
	event model.AgentEvent
}

func (w *WAL) segmentPath(num uint64) string {
	return filepath.Join(w.dir, fmt.Sprintf("%09d.wal", num))
}

func (w *WAL) checkpointPath() string {
	return filepath.Join(w.dir, "checkpoint.json")
}

func (w *WAL) loadCheckpoint() (checkpoint, error) {
	data, err := os.ReadFile(w.checkpointPath())
	if errors.Is(err, os.ErrNotExist) {
		return checkpoint{}, nil
	}
	if err != nil {
		return checkpoint{}, fmt.Errorf("wal: read checkpoint: %w", err)
	}
	var cp checkpoint
	if err := json.Unmarshal(data, &cp); err != nil {
		return checkpoint{}, fmt.Errorf("wal: parse checkpoint: %w", err)
	}
	return cp, nil
}

func (w *WAL) saveCheckpoint(cp checkpoint) error {
	data, err := json.Marshal(cp)
	if err != nil {
		return fmt.Errorf("wal: marshal checkpoint: %w", err)
	}

	tmp := w.checkpointPath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("wal: write checkpoint tmp: %w", err)
	}

	// Sync the temp file before rename for crash safety.
	f, err := os.Open(tmp) //nolint:gosec // path is constructed from w.dir
	if err != nil {
		return fmt.Errorf("wal: open checkpoint tmp for sync: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("wal: sync checkpoint tmp: %w", err)
	}
	_ = f.Close()

	if err := os.Rename(tmp, w.checkpointPath()); err != nil {
		return fmt.Errorf("wal: rename checkpoint: %w", err)
	}
	return nil
}

func (w *WAL) rotateSegment() error {
	if w.current != nil {
		if err := w.current.Sync(); err != nil {
			w.logger.Warn("wal: sync before rotation failed", "error", err)
		}
		if err := w.current.Close(); err != nil {
			w.logger.Warn("wal: close before rotation failed", "error", err)
		}
	}

	path := w.segmentPath(w.segmentNum)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // path is constructed from w.dir
	if err != nil {
		return fmt.Errorf("wal: open segment %d: %w", w.segmentNum, err)
	}

	// Write segment header.
	baseLSN := w.nextLSN.Load()
	var hdr [walHeaderSize]byte
	binary.BigEndian.PutUint32(hdr[0:4], walMagic)
	binary.BigEndian.PutUint16(hdr[4:6], walVersion)
	// hdr[6:8] reserved = 0
	binary.BigEndian.PutUint64(hdr[8:16], baseLSN)

	if _, err := f.Write(hdr[:]); err != nil {
		_ = f.Close()
		return fmt.Errorf("wal: write segment header: %w", err)
	}

	w.current = f
	w.segmentSize = walHeaderSize
	w.segmentRecs = 0
	w.segmentNum++
	return nil
}

func (w *WAL) listSegments() ([]string, error) {
	entries, err := os.ReadDir(w.dir)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".wal") {
			paths = append(paths, filepath.Join(w.dir, e.Name()))
		}
	}
	sort.Strings(paths) // lexicographic = numeric order due to zero-padding
	return paths, nil
}

func (w *WAL) highestSegment() (uint64, error) {
	entries, err := os.ReadDir(w.dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	var highest uint64
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".wal") {
			continue
		}
		var num uint64
		if _, err := fmt.Sscanf(name, "%09d.wal", &num); err == nil && num > highest {
			highest = num
		}
	}
	return highest, nil
}

func (w *WAL) readSegment(path string) ([]walRecord, uint64, error) {
	f, err := os.Open(path) //nolint:gosec // path is constructed from w.dir
	if err != nil {
		return nil, 0, fmt.Errorf("wal: open segment: %w", err)
	}
	defer f.Close() //nolint:errcheck // read-only file; close error is non-actionable

	// Read and validate header.
	var hdr [walHeaderSize]byte
	if _, err := io.ReadFull(f, hdr[:]); err != nil {
		return nil, 0, fmt.Errorf("wal: read segment header: %w", err)
	}
	magic := binary.BigEndian.Uint32(hdr[0:4])
	if magic != walMagic {
		return nil, 0, fmt.Errorf("wal: bad magic 0x%08X (expected 0x%08X)", magic, walMagic)
	}
	version := binary.BigEndian.Uint16(hdr[4:6])
	if version != walVersion {
		return nil, 0, fmt.Errorf("wal: unsupported version %d", version)
	}

	var records []walRecord
	var highLSN uint64

	for {
		var head [walRecordHead]byte
		_, err := io.ReadFull(f, head[:])
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			break // end of segment or truncated record
		}
		if err != nil {
			return records, highLSN, fmt.Errorf("wal: read record head: %w", err)
		}

		lsn := binary.BigEndian.Uint64(head[0:8])
		payloadLen := binary.BigEndian.Uint32(head[8:12])

		if payloadLen > walMaxPayload {
			// Corrupted length — stop reading this segment.
			w.logger.Warn("wal: corrupted payload length, stopping segment read",
				"path", path, "lsn", lsn, "payload_len", payloadLen)
			break
		}

		payload := make([]byte, payloadLen)
		if _, err := io.ReadFull(f, payload); err != nil {
			break // truncated record
		}

		var crcBuf [walCRCSize]byte
		if _, err := io.ReadFull(f, crcBuf[:]); err != nil {
			break // truncated CRC
		}

		// Verify CRC.
		h := crc32.New(crc32cTable)
		_, _ = h.Write(head[:])
		_, _ = h.Write(payload)
		expected := h.Sum32()
		actual := binary.BigEndian.Uint32(crcBuf[:])
		if expected != actual {
			w.logger.Warn("wal: CRC mismatch, stopping segment read",
				"path", path, "lsn", lsn, "expected_crc", expected, "actual_crc", actual)
			break
		}

		var event model.AgentEvent
		if err := json.Unmarshal(payload, &event); err != nil {
			w.logger.Warn("wal: corrupted event JSON, stopping segment read",
				"path", path, "lsn", lsn, "error", err)
			break
		}

		records = append(records, walRecord{lsn: lsn, event: event})
		if lsn > highLSN {
			highLSN = lsn
		}
	}

	return records, highLSN, nil
}

func (w *WAL) cleanupSegments(flushedLSN uint64) error {
	segments, err := w.listSegments()
	if err != nil {
		return err
	}

	for _, seg := range segments {
		_, highLSN, err := w.readSegment(seg)
		if err != nil {
			continue // skip unreadable segments
		}
		if highLSN > 0 && highLSN <= flushedLSN {
			if err := os.Remove(seg); err != nil {
				w.logger.Warn("wal: failed to delete flushed segment", "path", seg, "error", err)
			}
		}
	}
	return nil
}

func (w *WAL) syncLoop(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	defer close(w.syncDone)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.mu.Lock()
			if w.current != nil {
				if err := w.current.Sync(); err != nil {
					w.logger.Warn("wal: batch sync failed", "error", err)
				}
			}
			w.mu.Unlock()
		}
	}
}

// registerMetrics registers OTEL metrics for WAL health monitoring.
func (w *WAL) registerMetrics() {
	meter := telemetry.Meter("akashi/wal")

	_, _ = meter.Int64ObservableGauge("akashi.wal.segment_count",
		metric.WithDescription("Current number of WAL segment files"),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			o.Observe(int64(w.SegmentCount()))
			return nil
		}),
	)

	_, _ = meter.Int64ObservableGauge("akashi.wal.pending_bytes",
		metric.WithDescription("Approximate bytes in un-flushed WAL segments"),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			o.Observe(w.PendingBytes())
			return nil
		}),
	)
}
