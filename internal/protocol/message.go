package protocol

import "time"

const (
	TypeRegisterAgent = "register-agent"
	TypeStartSession  = "start-session"
	TypeResize        = "resize"
	// TypePaired is sent by the server to both ends once a session is paired.
	TypePaired = "paired"
	// TypeDiagnostics is sent by an agent after (re)connecting with the
	// connection events it kept while it was offline.
	TypeDiagnostics = "diagnostics"
	// TypeUpdateAgent is sent by the server to ask an agent to replace itself
	// with Version, downloaded from the server and checked against SHA256.
	TypeUpdateAgent = "update-agent"
)

// CapSelfUpdate is advertised by agents that understand TypeUpdateAgent.
const CapSelfUpdate = "self-update"

// Event is something an agent noticed about its own connection or machine.
type Event struct {
	At     time.Time `json:"at"`
	Kind   string    `json:"kind"`
	Detail string    `json:"detail,omitempty"`
}

type Message struct {
	Type      string `json:"type"`
	SessionID string `json:"session_id,omitempty"`
	// Kind tells what a start-session is for: empty is a shell, KindFiles a
	// file transfer channel.
	Kind string `json:"kind,omitempty"`
	Data []byte `json:"data,omitempty"`

	Rows uint16 `json:"rows,omitempty"`
	Cols uint16 `json:"cols,omitempty"`

	AgentID string `json:"agent_id,omitempty"`
	Token   string `json:"token,omitempty"`

	// Facts an agent reports when it registers.
	Hostname string `json:"hostname,omitempty"`
	OS       string `json:"os,omitempty"`
	Arch     string `json:"arch,omitempty"`
	Version  string `json:"version,omitempty"`

	// Capabilities lists what an agent can do, sent when it registers.
	Capabilities []string `json:"capabilities,omitempty"`
	// SHA256 is the hex digest of the binary named by a TypeUpdateAgent.
	SHA256 string `json:"sha256,omitempty"`

	// Events carries a TypeDiagnostics batch.
	Events []Event `json:"events,omitempty"`
}
