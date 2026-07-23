//go:build integration_testdb

// Assertion-store route ABSENCE pin against the PRODUCTION route tree.
//
// KNW-033 is an invariant over the ENTIRE application route set: no HTTP
// endpoint exposes assertions, graph nodes, edges, predicates, or provenance.
// A handler-level test cannot prove an absence claim — only enumerating the
// real router built by the real registerRoutes can. This file reuses the
// oauth_routes_boundary_test.go wire-chain harness (buildRouterForOAuthWiring)
// so the enumerated tree is exactly what run() would serve for each config
// shape.
package main

import (
	"os"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// assertionStoreStems are lowercase substrings that mark a route as exposing
// the assertion store. Substring (not whole-segment) matching is deliberate:
// a rename like /graph-nodes or /assertion-list must still trip the scan.
// Each stem is derived from the KNW-033 statement text and the internal
// package surface it names:
//
//   - "assert":     assertions themselves (internal/repository/assertion.go,
//     AssertService) — also covers "assertion"/"assertions".
//   - "graph":      the knowledge graph as a whole (buildGraphCore, the graph
//     wire stack).
//   - "node":       graph nodes (internal/repository/node.go).
//   - "edge":       graph edges (assertion subject→object links). This stem
//     is also a substring of "knowledge", which is intentional: a /knowledge
//     route would itself violate KNW-033's claim that the only client-visible
//     projection is the contact payload's derived fields.
//   - "predicate":  predicates (internal/repository/predicate.go).
//   - "provenance": assertion provenance, named directly by the spec text.
//
// False-positive audit (why substring matching is safe here): the current
// production route set was inspected route-by-route and NO legitimate path
// contains any of these stems. The known-route sanity assertions below keep
// the scan honest — an empty or gutted route table cannot silently pass.
var assertionStoreStems = []string{
	"assert",
	"graph",
	"node",
	"edge",
	"predicate",
	"provenance",
}

// requireNoAssertionStoreRoutes scans every registered route path for the
// forbidden stems and fails naming the offending route.
func requireNoAssertionStoreRoutes(t *testing.T, routes gin.RoutesInfo) {
	t.Helper()
	for _, route := range routes {
		lower := strings.ToLower(route.Path)
		for _, stem := range assertionStoreStems {
			assert.NotContains(t, lower, stem,
				"route %s %s exposes an assertion-store surface (forbidden stem %q) — the assertion store must be invisible to the API",
				route.Method, route.Path, stem)
		}
	}
}

// requireKnownRoutes guards the scan's own harness: the route table must be
// nonzero and contain known-legit routes, so a rename or registration failure
// that empties the tree cannot make the absence scan vacuously green.
func requireKnownRoutes(t *testing.T, routes gin.RoutesInfo, wantPresent []string) {
	t.Helper()
	require.NotEmpty(t, routes, "production route table must not be empty")
	registered := make(map[string]bool, len(routes))
	for _, route := range routes {
		registered[route.Method+" "+route.Path] = true
	}
	for _, want := range wantPresent {
		require.True(t, registered[want],
			"known-legit route %s must be present — its absence means the router under scan is not the production route tree", want)
	}
}

// requireRouteAbsent proves a config shape actually SHRINKS the route tree.
// The disabled shape asserts a sync-only route is missing: if the feature
// flag were ignored (both shapes serving the identical, always-enabled
// router), the absence scan alone would pass on both shapes and prove
// nothing about the minimal tree — this assertion fails that defect loudly.
func requireRouteAbsent(t *testing.T, routes gin.RoutesInfo, wantAbsent string) {
	t.Helper()
	for _, route := range routes {
		require.NotEqual(t, wantAbsent, route.Method+" "+route.Path,
			"sync-only route %s must be absent from this config shape — its presence means the feature flag did not shape the route tree", wantAbsent)
	}
}

// TestKnowledgeRouteAbsence_AssertionStoreInvisible enumerates the FULL
// production router per config shape and asserts no route path touches the
// assertion store. The enabled shape is the maximal route surface (external
// sync on, both providers configured — the largest tree run() can serve); the
// disabled shape pins the invariant on the minimal tree too.
func TestKnowledgeRouteAbsence_AssertionStoreInvisible(t *testing.T) {
	t.Parallel()
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	t.Run("maximal route surface exposes no assertion-store route", func(t *testing.T) {
		// spec: KNW-033
		t.Parallel()
		cfg := oauthWiringConfig(t)
		cfg.Features.EnableExternalSync = true
		router := buildRouterForOAuthWiring(t, cfg)

		routes := router.Routes()
		requireKnownRoutes(t, routes, []string{
			"GET /health",
			"GET /api/v1/contacts",
			// Sync-only route: proves this shape really is the maximal
			// (external-sync-enabled) surface, not the disabled tree.
			"GET /api/v1/auth/google/callback",
		})
		requireNoAssertionStoreRoutes(t, routes)
	})

	t.Run("external-sync-disabled surface exposes no assertion-store route", func(t *testing.T) {
		// spec: KNW-033
		t.Parallel()
		cfg := oauthWiringConfig(t)
		cfg.Features.EnableExternalSync = false
		router := buildRouterForOAuthWiring(t, cfg)

		routes := router.Routes()
		requireKnownRoutes(t, routes, []string{
			"GET /health",
			"GET /api/v1/contacts",
		})
		// Distinguishability guard: the disabled tree must really be the
		// minimal shape, not the enabled tree served twice.
		requireRouteAbsent(t, routes, "GET /api/v1/auth/google/callback")
		requireNoAssertionStoreRoutes(t, routes)
	})
}
