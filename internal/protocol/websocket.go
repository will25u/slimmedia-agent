package protocol

import "time"

// Centralized WebSocket wire protocol configuration constants shared across
// agent client and API server to prevent configuration drift.
const (
	WSMaxMessageSize = 1 << 20 // 1 MiB per frame
	WSPingPeriod     = 45 * time.Second
	WSPongWait       = 60 * time.Second
	WSWriteWait      = 10 * time.Second
)
