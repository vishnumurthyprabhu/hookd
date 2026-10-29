// Command testreceiver is a webhook receiver for development and testing. It
// checks each request with the official Standard Webhooks Go library, logs
// whether it verified, and answers with a fixed status. It is not deployed.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	standardwebhooks "github.com/standard-webhooks/standard-webhooks/libraries/go"
)

// maxBodyBytes is the most of a request body that is read. Hookd payloads
// are at most 256 KB, so this leaves room.
const maxBodyBytes = 1 << 20

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}

// run parses args, serves until SIGINT or SIGTERM and returns the exit code.
func run(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("testreceiver", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addr := fs.String("addr", "127.0.0.1:9000", "address to listen on")
	secret := fs.String("secret", "", "endpoint signing secret, whsec_... (required)")
	status := fs.Int("status", http.StatusOK, "HTTP status to answer every request with")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *secret == "" {
		fmt.Fprintln(stderr, "testreceiver: --secret is required")
		fs.Usage()
		return 2
	}
	if *status < 100 || *status > 599 {
		fmt.Fprintf(stderr, "testreceiver: --status %d is not an HTTP status\n", *status)
		return 2
	}
	wh, err := standardwebhooks.NewWebhook(*secret)
	if err != nil {
		fmt.Fprintf(stderr, "testreceiver: --secret: %v\n", err)
		return 2
	}

	log := slog.New(slog.NewTextHandler(stderr, nil))
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Error("listen", "err", err)
		return 1
	}
	log.Info("listening", "addr", ln.Addr().String(), "status", *status)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	srv := &http.Server{Handler: newHandler(wh, *status, log), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("serve", "err", err)
		return 1
	}
	log.Info("stopped")
	return 0
}

// newHandler returns a handler that verifies each request with wh, logs the
// outcome and answers with status.
func newHandler(wh *standardwebhooks.Webhook, status int, log *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
		if err != nil {
			log.Error("read body", "err", err)
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}
		attrs := []any{
			"id", r.Header.Get("webhook-id"),
			"type", eventType(body),
			"body", string(body),
		}
		if err := wh.Verify(body, r.Header); err != nil {
			log.Warn("not verified", append(attrs, "verified", false, "err", err)...)
		} else {
			log.Info("verified", append(attrs, "verified", true)...)
		}
		w.WriteHeader(status)
	})
}

// eventType returns the top-level "type" field of a JSON body, or "" if there
// is none. Hookd sends the stored payload as is, so the event type is only
// known when the sender put it in the payload.
func eventType(body []byte) string {
	var v struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(body, &v) != nil {
		return ""
	}
	return v.Type
}
