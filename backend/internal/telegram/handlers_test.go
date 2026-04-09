package telegram

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func ptrInt32(v int32) *int32 { return &v }

func TestEffectiveTracked_TrackedOverridesLarge(t *testing.T) {
	assert.True(t, EffectiveTracked("tracked", ptrInt32(200), 10))
}

func TestEffectiveTracked_IgnoredOverridesSmall(t *testing.T) {
	assert.False(t, EffectiveTracked("ignored", ptrInt32(3), 10))
}

func TestEffectiveTracked_AutoSmallGroup(t *testing.T) {
	assert.True(t, EffectiveTracked("auto", ptrInt32(5), 10))
}

func TestEffectiveTracked_AutoLargeGroup(t *testing.T) {
	assert.False(t, EffectiveTracked("auto", ptrInt32(50), 10))
}

func TestEffectiveTracked_AutoExactThreshold(t *testing.T) {
	assert.True(t, EffectiveTracked("auto", ptrInt32(10), 10))
}

func TestEffectiveTracked_AutoNilMemberCount(t *testing.T) {
	assert.True(t, EffectiveTracked("auto", nil, 10))
}
