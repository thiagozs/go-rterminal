package protocol

type Message struct {
	Type    string `json:"type"`
	AgentID string `json:"agent_id,omitempty"`
	Data    []byte `json:"data,omitempty"`
	Token   string `json:"token,omitempty"`
}
