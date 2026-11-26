package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/thiagozs/go-rterminal/internal/auth"
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
	rootCmd.Flags().String("auth-token", "", "Token de autenticacao para agentes e clientes (ou RTERMINAL_TOKEN)")
	rootCmd.Flags().String("tls-cert", "", "Caminho do certificado TLS (opcional)")
	rootCmd.Flags().String("tls-key", "", "Caminho da chave TLS (opcional)")
	rootCmd.Flags().Duration("session-timeout", 15*time.Second, "Timeout para pareamento de sessao")
	cobra.CheckErr(rootCmd.Execute())
}

func runServer(cmd *cobra.Command, _ []string) error {
	addr, _ := cmd.Flags().GetString("addr")
	flagToken, _ := cmd.Flags().GetString("auth-token")
	tlsCert, _ := cmd.Flags().GetString("tls-cert")
	tlsKey, _ := cmd.Flags().GetString("tls-key")
	sessionTimeout, _ := cmd.Flags().GetDuration("session-timeout")
	authToken, err := auth.ResolveToken(flagToken)
	if err != nil {
		return err
	}
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

	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	serveErr := make(chan error, 1)
	go func() {
		log.Printf("[server] escutando em %s", addr)
		if tlsCert != "" {
			serveErr <- server.ListenAndServeTLS(tlsCert, tlsKey)
			return
		}
		serveErr <- server.ListenAndServe()
	}()

	select {
	case err := <-serveErr:
		return fmt.Errorf("iniciar server: %w", err)
	case <-ctx.Done():
	}
	log.Println("[server] encerrando")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("encerrar server: %w", err)
	}
	return nil
}
