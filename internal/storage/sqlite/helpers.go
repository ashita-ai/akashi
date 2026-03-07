package sqlite

import (
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/pgvector/pgvector-go"
)

// IsDuplicateKey returns true if err is a SQLite UNIQUE constraint violation.
func (l *LiteDB) IsDuplicateKey(err error) bool {
	if err == nil {
		return false
	}
	// modernc.org/sqlite surfaces constraint violations with this message prefix.
	return strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// scanJSON scans a TEXT column containing JSON into dst (pointer to map, slice, etc.).
// If the column is NULL or empty, dst is left at its zero value.
func scanJSON(val sql.NullString, dst any) error {
	if !val.Valid || val.String == "" {
		return nil
	}
	return json.Unmarshal([]byte(val.String), dst)
}

// jsonStr marshals v to a JSON string suitable for SQLite TEXT columns.
// nil values produce "null", empty maps produce "{}".
func jsonStr(v any) string {
	if v == nil {
		return "null"
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// uuidStr returns the string representation of a UUID for storage in TEXT columns.
func uuidStr(id uuid.UUID) string {
	return id.String()
}

// nullUUIDStr returns a sql.NullString for an optional UUID.
func nullUUIDStr(id *uuid.UUID) sql.NullString {
	if id == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: id.String(), Valid: true}
}

// parseUUID parses a TEXT column into a uuid.UUID.
func parseUUID(s string) uuid.UUID {
	id, _ := uuid.Parse(s)
	return id
}

// parseNullUUID parses an optional TEXT column into *uuid.UUID.
func parseNullUUID(s sql.NullString) *uuid.UUID {
	if !s.Valid || s.String == "" {
		return nil
	}
	id, err := uuid.Parse(s.String)
	if err != nil {
		return nil
	}
	return &id
}

// parseTime parses a TEXT column (RFC 3339 or SQLite datetime format) into time.Time.
func parseTime(s string) time.Time {
	// Try RFC 3339 first (what we write), then SQLite's default datetime format.
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	if t, err := time.Parse("2006-01-02 15:04:05", s); err == nil {
		return t
	}
	return time.Time{}
}

// parseNullTime parses an optional TEXT column into *time.Time.
func parseNullTime(s sql.NullString) *time.Time {
	if !s.Valid || s.String == "" {
		return nil
	}
	t := parseTime(s.String)
	if t.IsZero() {
		return nil
	}
	return &t
}

// timeStr formats a time.Time as RFC 3339 for SQLite TEXT columns.
func timeStr(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

// nullTimeStr formats an optional time as a sql.NullString.
func nullTimeStr(t *time.Time) sql.NullString {
	if t == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: timeStr(*t), Valid: true}
}

// vectorToBlob encodes a pgvector.Vector as a little-endian float32 BLOB.
func vectorToBlob(v *pgvector.Vector) []byte {
	if v == nil {
		return nil
	}
	slice := v.Slice()
	buf := make([]byte, len(slice)*4)
	for i, f := range slice {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(f))
	}
	return buf
}

// blobToVector decodes a BLOB back into a pgvector.Vector.
func blobToVector(b []byte) *pgvector.Vector {
	if len(b) == 0 {
		return nil
	}
	if len(b)%4 != 0 {
		return nil
	}
	n := len(b) / 4
	floats := make([]float32, n)
	for i := range n {
		floats[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	v := pgvector.NewVector(floats)
	return &v
}

// placeholders returns a comma-separated string of N "?" placeholders.
func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.Repeat("?,", n-1) + "?"
}

// uuidSliceToJSON marshals a slice of UUIDs into a JSON array string for use
// with json_each() in SQLite queries.
func uuidSliceToJSON(ids []uuid.UUID) string {
	strs := make([]string, len(ids))
	for i, id := range ids {
		strs[i] = fmt.Sprintf("%q", id.String())
	}
	return "[" + strings.Join(strs, ",") + "]"
}
