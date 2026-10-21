package protocol

import (
	"encoding/json"
	"time"
)

// Envelope is the standard message format for Agent <-> Cloud communication.
type Envelope struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	TS        time.Time       `json:"ts"`
	AgentID   string          `json:"agent_id,omitempty"`
	AgentName string          `json:"agent_name,omitempty"`
	Payload   json.RawMessage `json:"payload,omitempty"`
}
