package anarlog

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"

	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type mockExternalUpserter struct {
	captured repository.UpsertExternalContactRequest
	calls    int
	err      error
}

func (m *mockExternalUpserter) UpsertTx(_ context.Context, _ pgx.Tx, req repository.UpsertExternalContactRequest) (*repository.ExternalContact, error) {
	m.calls++
	m.captured = req
	if m.err != nil {
		return nil, m.err
	}
	return &repository.ExternalContact{Source: req.Source, SourceID: req.SourceID}, nil
}

// TestComputeAnarlogTitleSourceID_Determinism — same inputs always
// yield the same hex hash.
func TestComputeAnarlogTitleSourceID_Determinism(t *testing.T) {
	sid := uuid.MustParse("8a4f2c1e-1234-5678-9abc-def012345678")
	first := computeAnarlogTitleSourceID("alice", sid)
	for i := 0; i < 25; i++ {
		got := computeAnarlogTitleSourceID("alice", sid)
		if got != first {
			t.Fatalf("non-deterministic: run 0 = %s, run %d = %s", first, i, got)
		}
	}
}

// TestComputeAnarlogTitleSourceID_SpecLiteralRecipe — hash matches a
// hand-computed sha256(token || session_uuid_string) with no
// separator. Regression test against drift in the recipe.
func TestComputeAnarlogTitleSourceID_SpecLiteralRecipe(t *testing.T) {
	sid := uuid.MustParse("8a4f2c1e-1234-5678-9abc-def012345678")
	concat := "alice" + sid.String()
	want := sha256.Sum256([]byte(concat))
	wantHex := hex.EncodeToString(want[:])

	got := computeAnarlogTitleSourceID("alice", sid)
	if got != wantHex {
		t.Errorf("source_id recipe drift:\n got  = %s\n want = %s", got, wantHex)
	}
}

// TestComputeAnarlogTitleSourceID_DifferentSession — same token,
// different sessions → different rows.
func TestComputeAnarlogTitleSourceID_DifferentSession(t *testing.T) {
	a := computeAnarlogTitleSourceID("alice", uuid.MustParse("8a4f2c1e-1234-5678-9abc-def012345678"))
	b := computeAnarlogTitleSourceID("alice", uuid.MustParse("9b5e3d2f-1234-5678-9abc-def012345678"))
	if a == b {
		t.Errorf("different sessions collided: %s", a)
	}
}

// TestComputeAnarlogTitleSourceID_DifferentToken — same session,
// different tokens → different rows.
func TestComputeAnarlogTitleSourceID_DifferentToken(t *testing.T) {
	sid := uuid.MustParse("8a4f2c1e-1234-5678-9abc-def012345678")
	a := computeAnarlogTitleSourceID("alice", sid)
	b := computeAnarlogTitleSourceID("bob", sid)
	if a == b {
		t.Errorf("different tokens collided: %s", a)
	}
}

// TestUpsertTitleCandidateTx_TitleCaseNormalization — caller passes
// "ALICE", writer stores "Alice" in display_name + metadata.token_display.
func TestUpsertTitleCandidateTx_TitleCaseNormalization(t *testing.T) {
	mock := &mockExternalUpserter{}
	w := NewDiscoveryWriter(mock)
	sid := uuid.New()
	err := w.UpsertTitleCandidateTx(context.Background(), nil, sid, "alice", "ALICE")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if mock.captured.DisplayName == nil || *mock.captured.DisplayName != "Alice" {
		t.Errorf("display_name not title-cased: %+v", mock.captured.DisplayName)
	}
	td, _ := mock.captured.Metadata["token_display"].(string)
	if td != "Alice" {
		t.Errorf("metadata.token_display not title-cased: %q", td)
	}
}

// TestUpsertTitleCandidateTx_MetadataShape — verifies the four keys
// the downstream UI contract depends on.
func TestUpsertTitleCandidateTx_MetadataShape(t *testing.T) {
	mock := &mockExternalUpserter{}
	w := NewDiscoveryWriter(mock)
	sid := uuid.New()
	err := w.UpsertTitleCandidateTx(context.Background(), nil, sid, "alice", "Alice")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	md := mock.captured.Metadata
	if md == nil {
		t.Fatalf("metadata was nil")
	}
	for _, key := range []string{"session_uuid", "token_normalized", "token_display", "extracted_at"} {
		if _, ok := md[key]; !ok {
			t.Errorf("metadata missing %q: got %+v", key, md)
		}
	}
	if md["session_uuid"] != sid.String() {
		t.Errorf("session_uuid mismatch: got %v", md["session_uuid"])
	}
	if md["token_normalized"] != "alice" {
		t.Errorf("token_normalized mismatch: got %v", md["token_normalized"])
	}
}

// TestUpsertTitleCandidateTx_SourceAndSourceID — verifies the request
// shape passed to the repo: source="anarlog_title", deterministic
// source_id.
func TestUpsertTitleCandidateTx_SourceAndSourceID(t *testing.T) {
	mock := &mockExternalUpserter{}
	w := NewDiscoveryWriter(mock)
	sid := uuid.MustParse("8a4f2c1e-1234-5678-9abc-def012345678")
	err := w.UpsertTitleCandidateTx(context.Background(), nil, sid, "alice", "Alice")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if mock.captured.Source != "anarlog_title" {
		t.Errorf("wrong source: %q", mock.captured.Source)
	}
	want := computeAnarlogTitleSourceID("alice", sid)
	if mock.captured.SourceID != want {
		t.Errorf("wrong source_id: got %q, want %q", mock.captured.SourceID, want)
	}
	if mock.captured.HostID != nil {
		t.Errorf("HostID must be nil: got %+v", mock.captured.HostID)
	}
	if mock.captured.LastContentHash != nil {
		t.Errorf("LastContentHash must be nil: got %+v", mock.captured.LastContentHash)
	}
}

// TestUpsertTitleCandidateTx_RepoError surfaces errors to the caller.
func TestUpsertTitleCandidateTx_RepoError(t *testing.T) {
	mock := &mockExternalUpserter{err: errors.New("boom")}
	w := NewDiscoveryWriter(mock)
	sid := uuid.New()
	err := w.UpsertTitleCandidateTx(context.Background(), nil, sid, "alice", "Alice")
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
}

// TestUpsertTitleCandidateTx_EmptyNormalizedToken — defensive guard:
// the caller is responsible for non-empty input.
func TestUpsertTitleCandidateTx_EmptyNormalizedToken(t *testing.T) {
	mock := &mockExternalUpserter{}
	w := NewDiscoveryWriter(mock)
	sid := uuid.New()
	err := w.UpsertTitleCandidateTx(context.Background(), nil, sid, "", "Alice")
	if err == nil {
		t.Fatalf("expected error for empty normalizedToken, got nil")
	}
	if mock.calls != 0 {
		t.Errorf("empty token should not invoke repo, got %d calls", mock.calls)
	}
}
