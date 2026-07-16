package main

import (
	"fmt"
	"net/http"
	"os"
	"time"
)

// healthCheck hits the local /healthz endpoint, exiting 0 if healthy. Used as the
// container HEALTHCHECK (the distroless image has no shell or curl).
func healthCheck() error {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://127.0.0.1:" + port + "/healthz")
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthz returned %d", resp.StatusCode)
	}
	return nil
}
