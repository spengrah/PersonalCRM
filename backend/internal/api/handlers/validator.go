package handlers

import "github.com/go-playground/validator/v10"

// sharedValidator is the single validator instance shared by every handler.
// validator.Validate is concurrency-safe (it caches struct metadata under a
// mutex), and no handler registers custom validations/aliases, so one instance
// is safe to share and avoids constructing nine identical validators at wire-up.
var sharedValidator = validator.New()
