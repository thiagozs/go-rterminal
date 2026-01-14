package protocol

type Message struct {
	Type string `json:"type"`

	// resize
	Cols uint16 `json:"cols,omitempty"`
	Rows uint16 `json:"rows,omitempty"`

	AgentID string `json:"agent_id,omitempty"`
	Token   string `json:"token,omitempty"`
}
