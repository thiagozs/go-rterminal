package mailer

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type captured struct {
	method, path, auth, contentType string
	body                            map[string]any
}

func fakeResend(t *testing.T, status int, reply string) (*httptest.Server, *captured) {
	t.Helper()
	got := &captured{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.method, got.path, got.auth, got.contentType = r.Method, r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("Content-Type")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &got.body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(reply))
	}))
	t.Cleanup(srv.Close)
	return srv, got
}

func TestResendSendsThePlainTextThroughTheAPI(t *testing.T) {
	srv, got := fakeResend(t, 200, `{"id":"49a3999c-0ce1-4ea6-ab68-afcd6dc2e794"}`)
	r, err := NewResend(ResendConfig{APIKey: "re_test_key", From: "Painel <painel@exemplo.com>", BaseURL: srv.URL + "/"})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Send("ana@exemplo.com", "Seu código de acesso", "Seu código: 123456\nvale 5 minutos"); err != nil {
		t.Fatal(err)
	}
	if got.method != "POST" || got.path != "/emails" || got.auth != "Bearer re_test_key" || !strings.HasPrefix(got.contentType, "application/json") {
		t.Fatalf("request = %+v", got)
	}
	to, _ := got.body["to"].([]any)
	if got.body["from"] != "Painel <painel@exemplo.com>" || len(to) != 1 || to[0] != "ana@exemplo.com" ||
		got.body["subject"] != "Seu código de acesso" || got.body["text"] != "Seu código: 123456\nvale 5 minutos" {
		t.Fatalf("body = %v", got.body)
	}
	if _, html := got.body["html"]; html {
		t.Error("the message must go as plain text")
	}
}

func TestResendAPIErrorsComeBackWithoutTheKey(t *testing.T) {
	srv, _ := fakeResend(t, 422, `{"statusCode":422,"name":"validation_error","message":"The from address is not verified"}`)
	r, _ := NewResend(ResendConfig{APIKey: "re_secret_key", From: "a@b.co", BaseURL: srv.URL + "/"})
	err := r.Send("x@y.co", "s", "b")
	if err == nil || !strings.Contains(err.Error(), "not verified") {
		t.Fatalf("err = %v, want the API message", err)
	}
	if strings.Contains(err.Error(), "re_secret_key") {
		t.Fatalf("the API key leaked into the error: %v", err)
	}
}

func TestResendRejectsUnsafeInputAndSettings(t *testing.T) {
	r, _ := NewResend(ResendConfig{APIKey: "k", From: "a@b.co", BaseURL: "http://127.0.0.1:1/"})
	for _, to := range []string{"", "x", "a@b.co, c@d.co", "Ana <ana@x.co>"} {
		if err := r.Send(to, "s", "b"); err == nil || !strings.Contains(err.Error(), "destinatário") {
			t.Errorf("to %q: %v", to, err)
		}
	}
	if err := r.Send("a@b.co", "x\r\nBcc: evil@x.co", "b"); err == nil {
		t.Error("newline in the subject accepted")
	}
	for _, cfg := range []ResendConfig{{}, {APIKey: "k"}, {APIKey: " ", From: "a@b.co"}, {APIKey: "k", From: "not an address"}} {
		if _, err := NewResend(cfg); err == nil {
			t.Errorf("config %+v accepted", cfg)
		}
	}
}
