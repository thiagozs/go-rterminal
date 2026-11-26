package auth

import (
	"errors"
	"net/http"
	"os"
)

// EnvToken is the environment variable consulted when --auth-token is not given.
const EnvToken = "RTERMINAL_TOKEN"

// InsecureDefault is the historical placeholder token, which is now rejected.
const InsecureDefault = "change-me"

// ResolveToken returns the flag value or, when empty, the RTERMINAL_TOKEN
// environment variable. It fails when no token is available or when the
// token is the well-known placeholder.
func ResolveToken(flagValue string) (string, error) {
	token := flagValue
	if token == "" {
		token = os.Getenv(EnvToken)
	}
	if token == "" {
		return "", errors.New("token de autenticação obrigatório: use --auth-token ou a variável " + EnvToken)
	}
	if token == InsecureDefault {
		return "", errors.New("o token \"" + InsecureDefault + "\" é inseguro; defina um token secreto")
	}
	return token, nil
}

// SetAuthorization adds the bearer token to header when token is not empty.
func SetAuthorization(header http.Header, token string) {
	if token != "" {
		header.Set("Authorization", "Bearer "+token)
	}
}
