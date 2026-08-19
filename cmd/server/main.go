package main

import (
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/spf13/cobra"
	"github.com/thiagozs/go-rterminal/internal/relay"
)

func main() {
	rootCmd := &cobra.Command{
		Use:           "server",
		Short:         "Inicia o servidor do go-rterminal",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          runServer,
	}
	rootCmd.Flags().String("addr", ":8080", "Endereco de escuta HTTP")
	rootCmd.Flags().String("auth-token", "change-me", "Token de autenticacao para agentes e clientes")
	rootCmd.Flags().String("tls-cert", "", "Caminho do certificado TLS (opcional)")
	rootCmd.Flags().String("tls-key", "", "Caminho da chave TLS (opcional)")
	rootCmd.Flags().Duration("session-timeout", 15*time.Second, "Timeout para pareamento de sessao")
	cobra.CheckErr(rootCmd.Execute())
}

func runServer(cmd *cobra.Command, _ []string) error {
	addr, _ := cmd.Flags().GetString("addr")
	authToken, _ := cmd.Flags().GetString("auth-token")
	tlsCert, _ := cmd.Flags().GetString("tls-cert")
	tlsKey, _ := cmd.Flags().GetString("tls-key")
	sessionTimeout, _ := cmd.Flags().GetDuration("session-timeout")
	if sessionTimeout < 0 {
		return fmt.Errorf("session-timeout não pode ser negativo")
	}
	if (tlsCert == "") != (tlsKey == "") {
		return fmt.Errorf("tls-cert e tls-key devem ser informados juntos")
	}

	handler := relay.New(relay.Config{
		AuthToken:      authToken,
		SessionTimeout: sessionTimeout,
	}).Handler()
	server := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	log.Printf("[server] escutando em %s", addr)
	if tlsCert != "" {
		if err := server.ListenAndServeTLS(tlsCert, tlsKey); err != nil {
			return fmt.Errorf("iniciar server TLS: %w", err)
		}
		return nil
	}
	if err := server.ListenAndServe(); err != nil {
		return fmt.Errorf("iniciar server: %w", err)
	}
	return nil
}
