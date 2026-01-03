package accelerated

import (
	"os"
	"strconv"
	"time"
)

// GetCurrentTime returns the current time, potentially accelerated for testing
func GetCurrentTime() time.Time {
	now := time.Now() //nolint:forbidigo // This is the wrapper implementation

	// Check for acceleration settings
	accelerationStr := os.Getenv("TIME_ACCELERATION")
	if accelerationStr == "" {
		return now
	}

	acceleration, err := strconv.Atoi(accelerationStr)
	if err != nil || acceleration <= 1 {
		return now
	}

	// Get base time from environment (stored as Unix timestamp)
	baseTimeStr := os.Getenv("TIME_BASE")
	if baseTimeStr == "" {
		return now
	}

	// Parse as Unix timestamp (set by SetTimeAcceleration in system handler)
	baseUnix, err := strconv.ParseInt(baseTimeStr, 10, 64)
	if err != nil {
		return now
	}
	baseTime := time.Unix(baseUnix, 0)

	// Calculate accelerated time
	elapsed := now.Sub(baseTime)
	acceleratedElapsed := time.Duration(int64(elapsed) * int64(acceleration))
	return baseTime.Add(acceleratedElapsed)
}
