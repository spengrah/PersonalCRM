package db

import "errors"

// Common database errors
var (
	ErrNotFound = errors.New("record not found")
)
