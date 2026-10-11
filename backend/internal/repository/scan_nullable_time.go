package repository

import (
	"fmt"
	"time"
)

// scanNullableTime converts a pgx-scanned interface{} aggregate (sqlc cannot
// type an uncast MIN/MAX of timestamptz as nullable) into *time.Time. A NULL
// aggregate scans as nil and returns nil; any other non-time.Time value is an
// error rather than a silent "no value".
func scanNullableTime(v any) (*time.Time, error) {
	switch t := v.(type) {
	case nil:
		return nil, nil
	case time.Time:
		return &t, nil
	default:
		return nil, fmt.Errorf("scan nullable time: unexpected type %T", v)
	}
}
