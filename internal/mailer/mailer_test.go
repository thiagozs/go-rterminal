package mailer

import (
	"bufio"
	"net"
	"strings"
	"testing"
	"time"
)

// fakeSMTP accepts one message without encryption or auth and returns it.
func fakeSMTP(t *testing.T) (port int, got <-chan string) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	out := make(chan string, 1)
	go func() {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		r := bufio.NewReader(conn)
		say := func(s string) { conn.Write([]byte(s + "\r\n")) }
		say("220 fake ESMTP")
		var data strings.Builder
		inData := false
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\r\n")
			if inData {
				if line == "." {
					inData = false
					out <- data.String()
					say("250 ok")
					continue
				}
				data.WriteString(line + "\n")
				continue
			}
			switch cmd := strings.ToUpper(line); {
			case strings.HasPrefix(cmd, "EHLO"):
				say("250 fake")
			case strings.HasPrefix(cmd, "MAIL"), strings.HasPrefix(cmd, "RCPT"):
				say("250 ok")
			case cmd == "DATA":
				inData = true
				say("354 go")
			case cmd == "QUIT":
				say("221 bye")
				return
			default:
				say("250 ok")
			}
		}
	}()
	return l.Addr().(*net.TCPAddr).Port, out
}

func TestSendDeliversAUtf8Message(t *testing.T) {
	port, got := fakeSMTP(t)
	m, err := New(Config{Host: "127.0.0.1", Port: port, From: "Painel <painel@exemplo.com>", Security: None})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Send("ana@exemplo.com", "Seu código de acesso", "Código: 123456\nválido por 5 minutos"); err != nil {
		t.Fatal(err)
	}
	select {
	case msg := <-got:
		for _, want := range []string{"From: \"Painel\" <painel@exemplo.com>", "To: ana@exemplo.com", "=?utf-8?q?Seu_c=C3=B3digo", "Código: 123456", "charset=utf-8"} {
			if !strings.Contains(msg, want) {
				t.Errorf("message lacks %q:\n%s", want, msg)
			}
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no message received")
	}
}

func TestRejectsUnsafeInputAndSettings(t *testing.T) {
	m, _ := New(Config{Host: "127.0.0.1", Port: 1, From: "a@b.co", Security: None})
	for _, to := range []string{"", "x", "a@b.co\r\nBcc: evil@x.co", "Ana <ana@x.co>"} {
		if err := m.Send(to, "s", "b"); err == nil || !strings.Contains(err.Error(), "destinatário") {
			t.Errorf("to %q: %v", to, err)
		}
	}
	if err := m.Send("a@b.co", "x\r\nBcc: evil@x.co", "b"); err == nil {
		t.Error("header injection through the subject was accepted")
	}
	for _, cfg := range []Config{{}, {Host: "h"}, {Host: "h", From: "not an address"}, {Host: "h", From: "a@b.co", Security: "ssl3"}} {
		if _, err := New(cfg); err == nil {
			t.Errorf("config %+v accepted", cfg)
		}
	}
}

func TestStartTLSIsRequiredWhenAsked(t *testing.T) {
	port, _ := fakeSMTP(t) // does not offer STARTTLS
	m, _ := New(Config{Host: "127.0.0.1", Port: port, From: "a@b.co", Security: StartTLS})
	if err := m.Send("x@y.co", "s", "b"); err == nil || !strings.Contains(err.Error(), "STARTTLS") {
		t.Fatalf("err = %v, want a STARTTLS refusal", err)
	}
}
