// Command clipper is the composition root: it reads the configuration, builds
// the adapters, injects them into the use cases, runs everything and shuts it
// down cleanly on SIGTERM/SIGINT.
//
// Subcommands:
//
//	run          start the service (default)
//	healthcheck  probe the local /healthz (used by the container HEALTHCHECK)
//	gpucheck     validate NVENC in ffmpeg and whisper word timestamps
//	backup DEST  write a consistent copy of the SQLite database (VACUUM INTO)
//	twitch-auth  obtain the Twitch user token (Device Code Flow)
//	probe LOGIN  capture a public live for a while and estimate the HLS latency
//	dryrun [ID]  transcribe and render a retained clip without Claude (no state change)
package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	cmd := "run"
	if len(args) > 0 {
		cmd, args = args[0], args[1:]
	}
	var err error
	switch cmd {
	case "run":
		err = runService(args, stderr)
	case "healthcheck":
		err = healthcheck(envOr("CLIPPER_HEALTH_URL", "http://127.0.0.1:8080/healthz"))
	case "gpucheck":
		err = gpuCheck(args, stdout)
	case "backup":
		err = backup(args, stdout)
	case "twitch-auth":
		err = twitchAuth(args, stdout)
	case "probe":
		err = probe(args, stdout)
	case "dryrun":
		err = dryRun(args, stdout)
	case "help", "-h", "--help":
		_, _ = fmt.Fprintln(stdout, "usage: clipper [run|healthcheck|gpucheck|backup DEST|twitch-auth|probe LOGIN|dryrun [CLIP_ID]]")
		return 0
	default:
		err = fmt.Errorf("unknown command %q", cmd)
	}
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "clipper %s: %v\n", cmd, err)
		return 1
	}
	return 0
}

func healthcheck(url string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
