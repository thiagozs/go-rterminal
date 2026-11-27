package diag

import (
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"syscall"

	"github.com/gorilla/websocket"
)

// DialError is a failed connection attempt, with the HTTP status the server
// (or the proxy in front of it) answered with, when there was one.
type DialError struct {
	Err    error
	Status int
}

func (e *DialError) Error() string { return e.Err.Error() }
func (e *DialError) Unwrap() error { return e.Err }

// NewDialError wraps err with the response of the failed handshake (may be nil).
func NewDialError(err error, resp *http.Response) error {
	d := &DialError{Err: err}
	if resp != nil {
		d.Status = resp.StatusCode
	}
	return d
}

// Describe turns a connection error into a short explanation of what went
// wrong, in Portuguese, for the history shown in the panel.
func Describe(err error) string {
	if err == nil {
		return ""
	}
	var dial *DialError
	if errors.As(err, &dial) && dial.Status > 0 {
		switch dial.Status {
		case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			return fmt.Sprintf("o servidor respondeu HTTP %d: o proxy está no ar, mas o servidor não (reiniciando ou fora do ar)", dial.Status)
		case http.StatusUnauthorized, http.StatusForbidden:
			return fmt.Sprintf("o servidor respondeu HTTP %d: credencial recusada", dial.Status)
		}
		return fmt.Sprintf("o servidor respondeu HTTP %d", dial.Status)
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		if dns.IsNotFound {
			return "falha de DNS: o nome do servidor não foi encontrado"
		}
		return "falha de DNS: sem resposta do resolvedor (rede fora ou sem DNS)"
	}
	switch {
	case errors.Is(err, syscall.ENETUNREACH):
		return "rede inacessível: a máquina está sem rota para a internet (Wi-Fi caído?)"
	case errors.Is(err, syscall.EHOSTUNREACH):
		return "servidor inacessível: sem rota até o servidor"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "conexão recusada: nada escutando no endereço do servidor"
	case errors.Is(err, syscall.ECONNRESET):
		return "conexão reiniciada pela rede ou pelo servidor"
	}
	var unknown x509.UnknownAuthorityError
	var invalid x509.CertificateInvalidError
	var host x509.HostnameError
	if errors.As(err, &unknown) || errors.As(err, &invalid) || errors.As(err, &host) {
		return "certificado TLS do servidor rejeitado: " + err.Error()
	}
	var closeErr *websocket.CloseError
	if errors.As(err, &closeErr) {
		switch closeErr.Code {
		case websocket.CloseAbnormalClosure:
			return "conexão interrompida sem aviso (rede caiu, proxy reiniciou ou servidor reiniciou)"
		case websocket.ClosePolicyViolation:
			return "o servidor encerrou a conexão: " + closeErr.Text
		}
		return fmt.Sprintf("o servidor fechou a conexão (código %d %s)", closeErr.Code, closeErr.Text)
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "tempo esgotado: o servidor parou de responder (rede lenta, caiu ou a máquina dormiu)"
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return "conexão encerrada pelo outro lado"
	}
	return err.Error()
}
