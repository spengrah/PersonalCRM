package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/config"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newSystemTimeTestRouter builds the two system-time routes on a real gin
// engine, through the same RegisterSystemRoutes call site production uses so
// the test cannot drift from the real path shape. SystemHandler's contactRepo
// is nil deliberately: neither GetSystemTime nor SetTimeAcceleration reads
// it, and a nil pointer makes an accidental repository read panic loudly
// instead of silently succeeding against a database this test does not have.
func newSystemTimeTestRouter(t *testing.T, env string) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	cfg := config.RuntimeConfig{CRMEnvironment: env}
	h := NewSystemHandler(nil, cfg)
	r := gin.New()
	RegisterSystemRoutes(&r.RouterGroup, h)
	return r
}

// systemTimeData decodes GET /system/time's data payload.
type systemTimeData struct {
	CurrentTime        time.Time `json:"current_time"`
	IsAccelerated      bool      `json:"is_accelerated"`
	AccelerationFactor int       `json:"acceleration_factor"`
	Environment        string    `json:"environment"`
	BaseTime           string    `json:"base_time"`
}

type systemTimeEnvelope struct {
	Success bool           `json:"success"`
	Data    systemTimeData `json:"data"`
	Error   *envelopeError `json:"error"`
}

type envelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// setAccelerationData decodes POST /system/time/acceleration's ad-hoc success
// body, built inline in SetTimeAcceleration rather than through
// api.SendSuccess, so it is not that envelope's shape.
type setAccelerationData struct {
	AccelerationFactor int       `json:"acceleration_factor"`
	AppliedAt          time.Time `json:"applied_at"`
}

type setAccelerationEnvelope struct {
	Success bool                `json:"success"`
	Data    setAccelerationData `json:"data"`
	Error   *envelopeError      `json:"error"`
}

func getSystemTime(t *testing.T, r *gin.Engine) (int, systemTimeEnvelope) {
	t.Helper()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/system/time", nil)
	r.ServeHTTP(w, req)
	var env systemTimeEnvelope
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env))
	return w.Code, env
}

func postAcceleration(t *testing.T, r *gin.Engine, body string) (int, setAccelerationEnvelope) {
	t.Helper()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/system/time/acceleration", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	var env setAccelerationEnvelope
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &env))
	return w.Code, env
}

// TestSystemTime_BootEnableReanchorDisableCycle drives the full lifecycle
// over HTTP in one ordered sequence, because each step's assertions depend on
// state the previous step left behind.
//
// spec: SET-037.enabling-anchors-base-atomically, SET-037.disabling-discards-accumulated-offset
func TestSystemTime_BootEnableReanchorDisableCycle(t *testing.T) {
	accelerated.Reset()
	t.Cleanup(accelerated.Reset)

	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	restore := accelerated.SetNowForTest(func() time.Time { return now })
	t.Cleanup(restore)

	r := newSystemTimeTestRouter(t, "test")

	// Boot state: unconfigured, wall clock.
	code, env := getSystemTime(t, r)
	assert.Equal(t, http.StatusOK, code)
	assert.False(t, env.Data.IsAccelerated)
	assert.Equal(t, 1, env.Data.AccelerationFactor)
	assert.Equal(t, "", env.Data.BaseTime)
	assert.True(t, env.Data.CurrentTime.Equal(now), "current_time = %v, want %v", env.Data.CurrentTime, now)
	assert.Equal(t, "test", env.Data.Environment)

	// Enable acceleration: the base anchors at the current instant.
	postCode, postEnv := postAcceleration(t, r, `{"factor": 60}`)
	assert.Equal(t, http.StatusOK, postCode)
	assert.Equal(t, 60, postEnv.Data.AccelerationFactor)
	// applied_at is the anchored base ConfigureNow returns. This fixture's
	// fake clock is whole-second already, so this check alone can't tell that
	// implementation apart from a rejected one (a fresh GetCurrentTime() read
	// taken after the anchor) — TestSetTimeAcceleration_AppliedAtIsTheAnchoredBase
	// below uses a fractional-second clock specifically so the two diverge.
	assert.True(t, postEnv.Data.AppliedAt.Equal(now), "applied_at = %v, want the anchored base %v", postEnv.Data.AppliedAt, now)
	assert.Zero(t, postEnv.Data.AppliedAt.Nanosecond(), "applied_at must be truncated to a whole second")

	code, env = getSystemTime(t, r)
	assert.Equal(t, http.StatusOK, code)
	assert.True(t, env.Data.IsAccelerated)
	assert.Equal(t, 60, env.Data.AccelerationFactor)
	require.NotEmpty(t, env.Data.BaseTime)
	baseTime, err := time.Parse(time.RFC3339, env.Data.BaseTime)
	require.NoError(t, err)
	assert.True(t, baseTime.Equal(now), "base_time = %v, want %v", baseTime, now)
	assert.True(t, env.Data.CurrentTime.Equal(now), "current_time = %v, want %v (zero elapsed since anchor)", env.Data.CurrentTime, now)

	// Advance the wall clock, then re-anchor at the new instant.
	now = now.Add(60 * time.Second)
	code, env = getSystemTime(t, r)
	assert.Equal(t, http.StatusOK, code)
	wantBeforeReanchor := baseTime.Add(3600 * time.Second) // 60 real seconds * factor 60
	assert.True(t, env.Data.CurrentTime.Equal(wantBeforeReanchor), "current_time = %v, want %v", env.Data.CurrentTime, wantBeforeReanchor)

	postCode, postEnv = postAcceleration(t, r, `{"factor": 60}`)
	assert.Equal(t, http.StatusOK, postCode)
	assert.Equal(t, 60, postEnv.Data.AccelerationFactor)

	code, env = getSystemTime(t, r)
	assert.Equal(t, http.StatusOK, code)
	reanchoredBase, err := time.Parse(time.RFC3339, env.Data.BaseTime)
	require.NoError(t, err)
	assert.True(t, reanchoredBase.Equal(now), "base_time after re-anchor = %v, want the advanced now %v", reanchoredBase, now)
	assert.True(t, env.Data.CurrentTime.Equal(reanchoredBase), "current_time after re-anchor = %v, want the new base %v exactly", env.Data.CurrentTime, reanchoredBase)

	// Disable acceleration: the accumulated offset is discarded, base cleared.
	postCode, postEnv = postAcceleration(t, r, `{"factor": 1}`)
	assert.Equal(t, http.StatusOK, postCode)
	assert.Equal(t, 1, postEnv.Data.AccelerationFactor)

	code, env = getSystemTime(t, r)
	assert.Equal(t, http.StatusOK, code)
	assert.False(t, env.Data.IsAccelerated)
	assert.Equal(t, 1, env.Data.AccelerationFactor)
	assert.Equal(t, "", env.Data.BaseTime)
	assert.True(t, env.Data.CurrentTime.Equal(now), "current_time after disable = %v, want the fake wall clock %v (accelerated offset discarded)", env.Data.CurrentTime, now)
}

// TestSetTimeAcceleration_AppliedAtIsTheAnchoredBase pins that applied_at is
// ConfigureNow's own anchored, whole-second-truncated base, not a value
// freshly recomputed by calling GetCurrentTime() again after anchoring. The
// fake clock deliberately carries a non-zero sub-second component: under
// acceleration, a fresh post-anchor GetCurrentTime() read would scale that
// discarded fraction by the factor and land somewhere else entirely, so a
// whole-second fixture (as used elsewhere in this file) cannot tell the two
// implementations apart — this one can.
//
// spec: SET-037.enabling-anchors-base-atomically
func TestSetTimeAcceleration_AppliedAtIsTheAnchoredBase(t *testing.T) {
	accelerated.Reset()
	t.Cleanup(accelerated.Reset)

	fractional := time.Date(2026, 3, 1, 12, 0, 0, 750_000_000, time.UTC)
	restore := accelerated.SetNowForTest(func() time.Time { return fractional })
	t.Cleanup(restore)

	r := newSystemTimeTestRouter(t, "test")

	postCode, postEnv := postAcceleration(t, r, `{"factor": 60}`)
	require.Equal(t, http.StatusOK, postCode)

	want := fractional.Truncate(time.Second)
	assert.True(t, postEnv.Data.AppliedAt.Equal(want), "applied_at = %v, want the anchored, truncated base %v — a fresh post-anchor GetCurrentTime() read would instead land at roughly %v (the discarded 750ms scaled by factor 60)", postEnv.Data.AppliedAt, want, want.Add(45*time.Second))
	assert.Zero(t, postEnv.Data.AppliedAt.Nanosecond(), "applied_at must be truncated to a whole second")

	// applied_at reporting the right value is not enough on its own — a
	// mutant that reports applied_at correctly while installing a DIFFERENT
	// factor or base (e.g. leaving the clock at factor 1) would still pass
	// the two assertions above. Confirm the installed clock itself: factor
	// 60 anchored at the SAME truncated instant applied_at reports.
	getCode, getEnv := getSystemTime(t, r)
	require.Equal(t, http.StatusOK, getCode)
	assert.True(t, getEnv.Data.IsAccelerated)
	assert.Equal(t, 60, getEnv.Data.AccelerationFactor)
	installedBase, err := time.Parse(time.RFC3339, getEnv.Data.BaseTime)
	require.NoError(t, err)
	assert.True(t, installedBase.Equal(want), "installed base = %v, want the same anchored base %v applied_at reported", installedBase, want)
}

// TestSystemTime_FactorEdgeCases pins the factor contract across the entire
// integer range: any factor <= 1, negative included, disables acceleration
// and clears the base, while the factor itself is always echoed back exactly
// as supplied.
//
// spec: SET-037.factor-echoed-verbatim, SET-037.non-positive-factor-disables-acceleration
func TestSystemTime_FactorEdgeCases(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name          string
		factor        int
		wantActive    bool
		wantBaseEmpty bool
	}{
		{"negative", -5, false, true},
		{"one", 1, false, true},
		{"two", 2, true, false},
		{"max widget factor", 1440, true, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			accelerated.Reset()
			t.Cleanup(accelerated.Reset)
			cur := now
			restore := accelerated.SetNowForTest(func() time.Time { return cur })
			t.Cleanup(restore)

			r := newSystemTimeTestRouter(t, "test")

			postCode, postEnv := postAcceleration(t, r, jsonFactorBody(tc.factor))
			assert.Equal(t, http.StatusOK, postCode)
			assert.Equal(t, tc.factor, postEnv.Data.AccelerationFactor)

			code, env := getSystemTime(t, r)
			assert.Equal(t, http.StatusOK, code)
			assert.Equal(t, tc.factor, env.Data.AccelerationFactor)
			assert.Equal(t, tc.wantActive, env.Data.IsAccelerated)
			if tc.wantBaseEmpty {
				assert.Equal(t, "", env.Data.BaseTime)
			} else {
				require.NotEmpty(t, env.Data.BaseTime)
				baseTime, err := time.Parse(time.RFC3339, env.Data.BaseTime)
				require.NoError(t, err)
				assert.True(t, baseTime.Equal(now), "base_time = %v, want %v", baseTime, now)
			}

			if tc.name == "max widget factor" {
				cur = cur.Add(1 * time.Second)
				code, env = getSystemTime(t, r)
				assert.Equal(t, http.StatusOK, code)
				want := now.Add(1440 * time.Second)
				assert.True(t, env.Data.CurrentTime.Equal(want), "current_time = %v, want %v (exact, no drift)", env.Data.CurrentTime, want)
			}
		})
	}
}

func jsonFactorBody(factor int) string {
	b, _ := json.Marshal(map[string]int{"factor": factor})
	return string(b)
}

// TestSetTimeAcceleration_MissingFactorIsBadRequest pins that AccelerationSettings.Factor's
// binding:"required" rejects Go's zero value, and that a rejected request never mutates the
// process clock.
//
// spec: SET-037.missing-factor-rejected-without-mutation
func TestSetTimeAcceleration_MissingFactorIsBadRequest(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

	t.Run("empty object", func(t *testing.T) {
		accelerated.Reset()
		t.Cleanup(accelerated.Reset)
		restore := accelerated.SetNowForTest(func() time.Time { return now })
		t.Cleanup(restore)
		r := newSystemTimeTestRouter(t, "test")

		code, env := postAcceleration(t, r, `{}`)
		assert.Equal(t, http.StatusBadRequest, code)
		assert.False(t, env.Success)
		require.NotNil(t, env.Error)
		assert.Equal(t, "VALIDATION_ERROR", env.Error.Code)
		assert.NotEmpty(t, env.Error.Message)

		getCode, getEnv := getSystemTime(t, r)
		assert.Equal(t, http.StatusOK, getCode)
		assert.False(t, getEnv.Data.IsAccelerated)
		assert.Equal(t, 1, getEnv.Data.AccelerationFactor)
	})

	t.Run("explicit zero", func(t *testing.T) {
		accelerated.Reset()
		t.Cleanup(accelerated.Reset)
		restore := accelerated.SetNowForTest(func() time.Time { return now })
		t.Cleanup(restore)
		r := newSystemTimeTestRouter(t, "test")

		code, env := postAcceleration(t, r, `{"factor": 0}`)
		assert.Equal(t, http.StatusBadRequest, code)
		assert.False(t, env.Success)
		require.NotNil(t, env.Error)
		assert.Equal(t, "VALIDATION_ERROR", env.Error.Code)

		getCode, getEnv := getSystemTime(t, r)
		assert.Equal(t, http.StatusOK, getCode)
		assert.False(t, getEnv.Data.IsAccelerated)
		assert.Equal(t, 1, getEnv.Data.AccelerationFactor)
	})

	t.Run("malformed json against an active clock", func(t *testing.T) {
		accelerated.Reset()
		t.Cleanup(accelerated.Reset)
		cur := now
		restore := accelerated.SetNowForTest(func() time.Time { return cur })
		t.Cleanup(restore)
		r := newSystemTimeTestRouter(t, "test")

		setCode, setEnv := postAcceleration(t, r, `{"factor": 60}`)
		require.Equal(t, http.StatusOK, setCode)
		require.Equal(t, 60, setEnv.Data.AccelerationFactor)

		beforeCode, beforeEnv := getSystemTime(t, r)
		require.Equal(t, http.StatusOK, beforeCode)
		require.NotEmpty(t, beforeEnv.Data.BaseTime)

		// Advance the clock before the rejected request: if a bug silently
		// re-anchored the base on rejection, the base_time comparison below
		// would only catch it if the anchor instant actually moved.
		cur = cur.Add(10 * time.Second)

		code, env := postAcceleration(t, r, `{"factor":`)
		assert.Equal(t, http.StatusBadRequest, code)
		assert.False(t, env.Success)
		require.NotNil(t, env.Error)
		assert.Equal(t, "VALIDATION_ERROR", env.Error.Code)

		// The unchanged-clock assertion checked against an ACTIVE clock, not
		// only the reset state: the rejected malformed request must not have
		// disturbed the factor 60 configured just above. Pin the base too, not
		// just factor/is_accelerated — a regression that re-anchored the base
		// on a rejected request while leaving the factor untouched would pass
		// factor/is_accelerated-only assertions.
		getCode, getEnv := getSystemTime(t, r)
		assert.Equal(t, http.StatusOK, getCode)
		assert.True(t, getEnv.Data.IsAccelerated)
		assert.Equal(t, 60, getEnv.Data.AccelerationFactor)
		assert.Equal(t, beforeEnv.Data.BaseTime, getEnv.Data.BaseTime, "base_time must be unchanged by a rejected request")
	})
}

// TestSystemTime_FactorWithoutBaseReportsInactive pins that activation
// requires a factor > 1 AND a usable base: a factor with no parseable base
// reports is_accelerated: false rather than true with an unusable empty
// base_time, because a client cannot compute a clock from a base it never
// received. This state is unreachable through the setter (ConfigureNow
// always anchors a base) and reachable only through boot config, so it is
// driven via accelerated.ConfigureAtBoot directly.
//
// spec: SET-037.factor-without-usable-base-reports-inactive
func TestSystemTime_FactorWithoutBaseReportsInactive(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

	t.Run("no base", func(t *testing.T) {
		accelerated.Reset()
		t.Cleanup(accelerated.Reset)
		restore := accelerated.SetNowForTest(func() time.Time { return now })
		t.Cleanup(restore)

		err := accelerated.ConfigureAtBoot(60, "")
		require.Error(t, err)

		r := newSystemTimeTestRouter(t, "test")
		code, env := getSystemTime(t, r)
		assert.Equal(t, http.StatusOK, code)
		assert.Equal(t, 60, env.Data.AccelerationFactor)
		assert.False(t, env.Data.IsAccelerated, "delta: today's handler reports true here")
		assert.Equal(t, "", env.Data.BaseTime)
		assert.True(t, env.Data.CurrentTime.Equal(now), "current_time = %v, want the wall clock %v", env.Data.CurrentTime, now)
	})

	t.Run("unparseable base", func(t *testing.T) {
		accelerated.Reset()
		t.Cleanup(accelerated.Reset)
		restore := accelerated.SetNowForTest(func() time.Time { return now })
		t.Cleanup(restore)

		err := accelerated.ConfigureAtBoot(60, "not-a-timestamp")
		require.Error(t, err)

		r := newSystemTimeTestRouter(t, "test")
		code, env := getSystemTime(t, r)
		assert.Equal(t, http.StatusOK, code)
		assert.Equal(t, 60, env.Data.AccelerationFactor)
		assert.False(t, env.Data.IsAccelerated)
		assert.Equal(t, "", env.Data.BaseTime)
		assert.True(t, env.Data.CurrentTime.Equal(now))
	})
}

// TestSystemTime_SingleLoadSnapshot proves the response GetSystemTime returns
// is always self-consistent — current_time and the factor/base/is_accelerated
// beside it always describe the same configuration. It forces a
// reconfiguration to land BETWEEN a two-load handler's two loads by hooking
// the package clock: the hook reconfigures the package the first time it is
// called (guarded by sync.Once) and then returns a fixed instant on every
// call thereafter. A single-load handler (SnapshotWithTime) captures its
// settings before ever calling nowFn, so the reconfiguration lands after the
// response is already determined and the response stays self-consistent. A
// two-load handler (GetCurrentTime() then Snapshot()) computes current_time
// under the pre-hook configuration but reads factor/base/active under the
// post-hook one, producing a torn response. The assertion recomputes the
// clock from the response's OWN factor/base/is_accelerated and requires
// exact equality against the response's current_time — a self-consistent
// response satisfies this by construction; a torn one cannot.
//
// spec: SET-037.response-reflects-one-configuration
func TestSystemTime_SingleLoadSnapshot(t *testing.T) {
	fixed := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name       string
		configureA func()
		configureB func()
	}{
		{
			name:       "slow_to_fast",
			configureA: func() { accelerated.Configure(2, fixed.Add(-1000*time.Second)) },
			configureB: func() { accelerated.Configure(1000, fixed.Add(-500*time.Second)) },
		},
		{
			name:       "fast_to_slow",
			configureA: func() { accelerated.Configure(1000, fixed.Add(-1000*time.Second)) },
			configureB: func() { accelerated.Configure(2, fixed.Add(-500*time.Second)) },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			accelerated.Reset()
			t.Cleanup(accelerated.Reset)

			tc.configureA()

			var once sync.Once
			restore := accelerated.SetNowForTest(func() time.Time {
				once.Do(tc.configureB)
				return fixed
			})
			t.Cleanup(restore)

			r := newSystemTimeTestRouter(t, "test")
			code, env := getSystemTime(t, r)
			assert.Equal(t, http.StatusOK, code)

			var want time.Time
			if env.Data.IsAccelerated {
				baseTime, err := time.Parse(time.RFC3339, env.Data.BaseTime)
				require.NoError(t, err)
				want = baseTime.Add(fixed.Sub(baseTime) * time.Duration(env.Data.AccelerationFactor))
			} else {
				want = fixed
			}
			assert.True(t, env.Data.CurrentTime.Equal(want), "current_time = %v, want %v recomputed from the response's own factor/base/is_accelerated (a torn response fails this)", env.Data.CurrentTime, want)
		})
	}
}
