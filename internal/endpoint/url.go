package endpoint

import (
	"fmt"
	"net/url"
	"strings"
)

// URL resolves an API path against base and safely encodes its query parameters.
func URL(base, apiPath string, query url.Values, websocket bool) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("URL do servidor inválida: %w", err)
	}
	if u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("URL do servidor deve conter esquema e host")
	}
	if websocket {
		switch u.Scheme {
		case "http":
			u.Scheme = "ws"
		case "https":
			u.Scheme = "wss"
		case "ws", "wss":
		default:
			return "", fmt.Errorf("esquema %q não suporta WebSocket", u.Scheme)
		}
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/" + strings.TrimLeft(apiPath, "/")
	u.RawQuery = query.Encode()
	return u.String(), nil
}
