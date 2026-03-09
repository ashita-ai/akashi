package sqlite

import (
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseTime_RFC3339Nano(t *testing.T) {
	ts := "2025-06-15T10:30:45.123456789Z"
	got := parseTime(ts)
	assert.Equal(t, 2025, got.Year())
	assert.Equal(t, time.June, got.Month())
	assert.Equal(t, 15, got.Day())
	assert.Equal(t, 10, got.Hour())
	assert.Equal(t, 30, got.Minute())
	assert.Equal(t, 45, got.Second())
}

func TestParseTime_RFC3339(t *testing.T) {
	ts := "2025-06-15T10:30:45Z"
	got := parseTime(ts)
	assert.Equal(t, 2025, got.Year())
	assert.Equal(t, time.June, got.Month())
	assert.Equal(t, 15, got.Day())
}

func TestParseTime_SQLiteDatetime(t *testing.T) {
	ts := "2025-06-15 10:30:45"
	got := parseTime(ts)
	assert.Equal(t, 2025, got.Year())
	assert.Equal(t, time.June, got.Month())
	assert.Equal(t, 15, got.Day())
	assert.Equal(t, 10, got.Hour())
}

func TestParseTime_Invalid(t *testing.T) {
	got := parseTime("not-a-date")
	assert.True(t, got.IsZero(), "invalid time string should return zero time")
}

func TestParseNullTime_NullString(t *testing.T) {
	got := parseNullTime(sql.NullString{Valid: false})
	assert.Nil(t, got)
}

func TestParseNullTime_EmptyString(t *testing.T) {
	got := parseNullTime(sql.NullString{Valid: true, String: ""})
	assert.Nil(t, got)
}

func TestParseNullTime_ValidTime(t *testing.T) {
	got := parseNullTime(sql.NullString{Valid: true, String: "2025-06-15T10:30:45Z"})
	require.NotNil(t, got)
	assert.Equal(t, 2025, got.Year())
}

func TestParseNullTime_InvalidTimeReturnsNil(t *testing.T) {
	// parseTime returns zero time for garbage input, and parseNullTime
	// maps zero time to nil.
	got := parseNullTime(sql.NullString{Valid: true, String: "garbage"})
	assert.Nil(t, got, "unparseable time string should return nil")
}

func TestNullTimeStr_Nil(t *testing.T) {
	got := nullTimeStr(nil)
	assert.False(t, got.Valid)
}

func TestNullTimeStr_NonNil(t *testing.T) {
	ts := time.Date(2025, 6, 15, 10, 30, 45, 0, time.UTC)
	got := nullTimeStr(&ts)
	assert.True(t, got.Valid)
	assert.Contains(t, got.String, "2025-06-15")
}

func TestScanJSON_NullInput(t *testing.T) {
	var dst map[string]any
	err := scanJSON(sql.NullString{Valid: false}, &dst)
	assert.NoError(t, err)
	assert.Nil(t, dst)
}

func TestScanJSON_EmptyString(t *testing.T) {
	var dst map[string]any
	err := scanJSON(sql.NullString{Valid: true, String: ""}, &dst)
	assert.NoError(t, err)
	assert.Nil(t, dst)
}

func TestScanJSON_ValidJSON(t *testing.T) {
	var dst map[string]any
	err := scanJSON(sql.NullString{Valid: true, String: `{"key":"val"}`}, &dst)
	assert.NoError(t, err)
	assert.Equal(t, "val", dst["key"])
}

func TestScanJSON_InvalidJSON(t *testing.T) {
	var dst map[string]any
	err := scanJSON(sql.NullString{Valid: true, String: `{not valid json}`}, &dst)
	assert.Error(t, err)
}

func TestJsonStr_Nil(t *testing.T) {
	assert.Equal(t, "null", jsonStr(nil))
}

func TestJsonStr_Map(t *testing.T) {
	got := jsonStr(map[string]any{"a": 1})
	assert.Contains(t, got, `"a"`)
}

func TestJsonStr_UnmarshalableValue(t *testing.T) {
	// Channels cannot be marshaled to JSON.
	ch := make(chan int)
	got := jsonStr(ch)
	assert.Equal(t, "{}", got)
}

func TestPlaceholders_Zero(t *testing.T) {
	assert.Equal(t, "", placeholders(0))
}

func TestPlaceholders_Negative(t *testing.T) {
	assert.Equal(t, "", placeholders(-1))
}

func TestPlaceholders_One(t *testing.T) {
	assert.Equal(t, "?", placeholders(1))
}

func TestPlaceholders_Three(t *testing.T) {
	assert.Equal(t, "?,?,?", placeholders(3))
}

func TestBlobToVector_Nil(t *testing.T) {
	assert.Nil(t, blobToVector(nil))
}

func TestBlobToVector_EmptySlice(t *testing.T) {
	assert.Nil(t, blobToVector([]byte{}))
}

func TestBlobToVector_NonAligned(t *testing.T) {
	// 3 bytes is not divisible by 4.
	assert.Nil(t, blobToVector([]byte{1, 2, 3}))
}

func TestParseNullUUID_Invalid(t *testing.T) {
	got := parseNullUUID(sql.NullString{Valid: false})
	assert.Nil(t, got)
}

func TestParseNullUUID_Empty(t *testing.T) {
	got := parseNullUUID(sql.NullString{Valid: true, String: ""})
	assert.Nil(t, got)
}

func TestParseNullUUID_BadFormat(t *testing.T) {
	got := parseNullUUID(sql.NullString{Valid: true, String: "not-a-uuid"})
	assert.Nil(t, got)
}

func TestParseNullUUID_Valid(t *testing.T) {
	id := uuid.New()
	got := parseNullUUID(sql.NullString{Valid: true, String: id.String()})
	require.NotNil(t, got)
	assert.Equal(t, id, *got)
}

func TestNullUUIDStr_Nil(t *testing.T) {
	got := nullUUIDStr(nil)
	assert.False(t, got.Valid)
}

func TestNullUUIDStr_NonNil(t *testing.T) {
	id := uuid.New()
	got := nullUUIDStr(&id)
	assert.True(t, got.Valid)
	assert.Equal(t, id.String(), got.String)
}

func TestUUIDSliceToJSON(t *testing.T) {
	id1 := uuid.MustParse("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
	id2 := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	got := uuidSliceToJSON([]uuid.UUID{id1, id2})
	assert.Contains(t, got, id1.String())
	assert.Contains(t, got, id2.String())
	assert.True(t, got[0] == '[' && got[len(got)-1] == ']')
}

func TestTimeStr(t *testing.T) {
	ts := time.Date(2025, 6, 15, 10, 30, 45, 0, time.UTC)
	got := timeStr(ts)
	assert.Contains(t, got, "2025-06-15")
	assert.Contains(t, got, "10:30:45")
}

func TestSanitizeOrderCol_ValidColumns(t *testing.T) {
	validCols := []string{
		"valid_from", "created_at", "confidence",
		"completeness_score", "outcome_score", "decision_type",
	}
	for _, col := range validCols {
		assert.Equal(t, col, sanitizeOrderCol(col), "valid column %q should pass through", col)
	}
}

func TestSanitizeOrderCol_Invalid(t *testing.T) {
	assert.Equal(t, "valid_from", sanitizeOrderCol("malicious_column"))
	assert.Equal(t, "valid_from", sanitizeOrderCol(""))
	assert.Equal(t, "valid_from", sanitizeOrderCol("DROP TABLE"))
}

func TestVectorToBlob_Nil(t *testing.T) {
	assert.Nil(t, vectorToBlob(nil))
}

func TestParseUUID_Invalid(t *testing.T) {
	got := parseUUID("not-valid")
	assert.Equal(t, uuid.Nil, got)
}
