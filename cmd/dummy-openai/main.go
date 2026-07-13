// dummy-openai is a deterministic, non-AI protocol provider for Thornhill's
// integration tests. It binds loopback by default and exits cleanly on SIGTERM.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"thornhill/internal/dummyopenai"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:0", "loopback listen address")
	token := flag.String("token", "", "optional bearer token")
	flag.Parse()

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	baseURL := "http://" + ln.Addr().String()
	endpoint, err := dummyopenai.EndpointJSON(baseURL)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(string(endpoint))

	srv := &http.Server{
		Handler:           dummyopenai.New(*token).Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       time.Minute,
		MaxHeaderBytes:    1 << 20,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
