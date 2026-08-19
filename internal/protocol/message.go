package protocol

const (
	TypeRegisterAgent = "register-agent"
	TypeStartSession  = "start-session"
	TypeResize        = "resize"
)

type Message struct {
	Type      string `json:"type"`
	SessionID string `json:"session_id,omitempty"`
	Data      []byte `json:"data,omitempty"`

	Rows uint16 `json:"rows,omitempty"`
	Cols uint16 `json:"cols,omitempty"`

	AgentID string `json:"agent_id,omitempty"`
	Token   string `json:"token,omitempty"`
}
