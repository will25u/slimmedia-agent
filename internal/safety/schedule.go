package safety

import (
	"time"

	"github.com/will25u/slimmedia-agent/internal/protocol"
)

// IsQuietHoursActive determines if current time is within the configured quiet hours window.
func IsQuietHoursActive(t time.Time, sched *protocol.ScheduleConfig) bool {
	return sched.IsQuietHoursActive(t)
}
