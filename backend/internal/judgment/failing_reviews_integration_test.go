//go:build integration_testdb

package judgment_test

import (
	"context"
	"errors"
	"sync"

	"personal-crm/backend/internal/judgment"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"

	"github.com/jackc/pgx/v5"
)

// failingReviews fails one review assertion once armed: the one a count of
// review writes reaches, so a write fails after recording earlier labels.
type failingReviews struct {
	judgment.Asserter
	mu   sync.Mutex
	left int
}

func (f *failingReviews) failOn(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.left = n
}

func (f *failingReviews) AssertTx(ctx context.Context, tx pgx.Tx, req service.AssertRequest) (*repository.Assertion, error) {
	if req.PredicateKey == "judgment_review" {
		f.mu.Lock()
		fail := false
		if f.left > 0 {
			f.left--
			fail = f.left == 0
		}
		f.mu.Unlock()
		if fail {
			return nil, errors.New("injected review failure")
		}
	}
	return f.Asserter.AssertTx(ctx, tx, req)
}
