package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/grendalaget/varde-plugin-autotournament/internal/plugin"
	"github.com/grendalaget/varde-plugin-autotournament/internal/state"
	"github.com/grendalaget/varde-plugin-autotournament/internal/varde"
)

var version = "dev"

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--version" {
		fmt.Println(version)
		return
	}
	if err := run(); err != nil {
		slog.Error("Auto Tournament plugin stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	slog.Info("Auto Tournament plugin starting", "version", version)
	if _, ok := os.LookupEnv("READYUP_LICENSE"); ok {
		slog.Warn("READYUP_LICENSE is ignored; configure readyup_license in each group")
	}
	secret := []byte(os.Getenv("VARDE_PLUGIN_SECRET"))
	if len(secret) < 32 {
		return errors.New("VARDE_PLUGIN_SECRET must contain at least 32 bytes")
	}
	vardeURL := strings.TrimSpace(os.Getenv("VARDE_URL"))
	serviceToken := strings.TrimSpace(os.Getenv("VARDE_SERVICE_TOKEN"))
	if vardeURL == "" || serviceToken == "" {
		return errors.New("VARDE_URL and VARDE_SERVICE_TOKEN are required")
	}
	stateDir := strings.TrimSpace(os.Getenv("STATE_DIR"))
	if stateDir == "" {
		return errors.New("STATE_DIR is required")
	}
	listen := strings.TrimSpace(os.Getenv("LISTEN"))
	if listen == "" {
		listen = "127.0.0.1:7310"
	}
	allowPrivateAT, err := envBool("AT_ALLOW_PRIVATE", false)
	if err != nil {
		return err
	}
	store, err := state.Open(stateDir, secret)
	if err != nil {
		return err
	}
	vardeClient, err := varde.NewClient(vardeURL, serviceToken)
	if err != nil {
		return fmt.Errorf("configure Varde client: %w", err)
	}
	readyUp, err := plugin.ReadyUpPinFromEnv()
	if err != nil {
		return err
	}
	if readyUp == nil {
		slog.Info("Ready Up addon management disabled")
	}
	service, err := plugin.New(plugin.Config{
		Secret:         secret,
		AllowPrivateAT: allowPrivateAT,
		State:          store,
		Varde:          vardeClient,
		ReadyUp:        readyUp,
	})
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := service.Start(ctx); err != nil {
		return err
	}

	server := &http.Server{
		Addr:              listen,
		Handler:           service.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	result := make(chan error, 1)
	go func() {
		result <- server.ListenAndServe()
	}()
	slog.Info("Auto Tournament plugin listening", "address", listen)
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shut down HTTP server: %w", err)
		}
		service.Wait()
		return nil
	case err := <-result:
		if errors.Is(err, http.ErrServerClosed) {
			service.Wait()
			return nil
		}
		return fmt.Errorf("serve plugin: %w", err)
	}
}

func envBool(name string, defaultValue bool) (bool, error) {
	raw, ok := os.LookupEnv(name)
	if !ok || strings.TrimSpace(raw) == "" {
		return defaultValue, nil
	}
	value, err := strconv.ParseBool(strings.TrimSpace(raw))
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean", name)
	}
	return value, nil
}
