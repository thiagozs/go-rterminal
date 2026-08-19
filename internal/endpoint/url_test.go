package endpoint

import (
	"net/url"
	"testing"
)

func TestURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		base      string
		path      string
		websocket bool
		want      string
	}{
		{"http api", "http://localhost:8080", "/client", false, "http://localhost:8080/client?agent=a%2Fb"},
		{"https websocket", "https://example.com/api/", "session", true, "wss://example.com/api/session?agent=a%2Fb"},
		{"ws remains ws", "ws://localhost:8080", "/agent", true, "ws://localhost:8080/agent?agent=a%2Fb"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := URL(test.base, test.path, url.Values{"agent": {"a/b"}}, test.websocket)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("URL() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestURLRejectsIncompleteBase(t *testing.T) {
	t.Parallel()
	if _, err := URL("localhost:8080", "/agent", nil, true); err == nil {
		t.Fatal("URL() should reject a base without scheme and host")
	}
}
