package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"
	"time"

	spool "github.com/plaonn/knit-spool-go"
)

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

const usage = "usage: knit-spool [run|check|version|commons-invite [--link-from-stdin]]"

// version is set by the release build (-ldflags "-X main.version=vX.Y.Z"); other builds report "dev".
var version = "dev"

// versionLine is `knit-spool <version> <commit>`: the updater reads the second field, people the third.
func versionLine() string {
	revision, modified := "unknown", ""
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			switch setting.Key {
			case "vcs.revision":
				revision = setting.Value
			case "vcs.modified":
				if setting.Value == "true" {
					modified = "+dirty"
				}
			}
		}
	}
	return fmt.Sprintf("knit-spool %s %s%s", version, revision, modified)
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	command := "run"
	if len(args) > 0 {
		command = args[0]
		args = args[1:]
	}
	switch command {
	case "version":
		if len(args) != 0 {
			return errors.New(usage)
		}
		fmt.Fprintln(stdout, versionLine())
		return nil
	case "check":
		if len(args) != 0 {
			return errors.New(usage)
		}
		cfg, err := spool.LoadConfig()
		if err != nil {
			return err
		}
		fmt.Fprintln(stdout, cfg.String())
		for _, warning := range cfg.Warnings() {
			fmt.Fprintln(stderr, "warning:", warning)
		}
		return nil
	case "commons-invite":
		switch {
		case len(args) == 0:
			return commonsInvite(stdout, stderr)
		case len(args) == 1 && args[0] == "--link-from-stdin":
			return relayLinkFromStdin(stdin, stdout, stderr)
		default:
			return errors.New(usage)
		}
	case "run":
		if len(args) != 0 {
			return errors.New(usage)
		}
	default:
		return fmt.Errorf("unknown command %q; %s", command, usage)
	}
	cfg, err := spool.LoadConfig()
	if err != nil {
		return err
	}
	engine, err := spool.NewEngine(cfg)
	if err != nil {
		return fmt.Errorf("open spool store: %w", err)
	}
	server := spool.NewServer(cfg, engine)
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	var workers sync.WaitGroup
	if cfg.SweepInterval > 0 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			runTicker(ctx, cfg.SweepInterval, func() {
				if err := engine.Sweep(); err != nil {
					log.Printf("sweep failed: %v", err)
				}
			})
		}()
	}
	if cfg.StatusInterval > 0 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			runTicker(ctx, cfg.StatusInterval, func() { log.Printf("status %v", engine.Stats()) })
		}()
	}
	log.Printf("starting %s (%s)", versionLine(), cfg.String())
	for _, warning := range cfg.Warnings() {
		log.Printf("warning: %s", warning)
	}
	serverErr := server.Run(ctx)
	cancel()
	workers.Wait()
	closeErr := engine.Close()
	if serverErr != nil {
		return serverErr
	}
	if closeErr != nil {
		return fmt.Errorf("close spool store: %w", closeErr)
	}
	return nil
}

func runTicker(ctx context.Context, interval time.Duration, fn func()) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			fn()
		}
	}
}

const linkWarning = "the link carries the room secret and, when SPOOL_TOKEN is set, this relay's access token; share it only with people allowed to join"

// commonsInvite mints a new commons secret, and a relay invite link for it when SPOOL_PUBLIC_URL is set.
func commonsInvite(stdout, stderr io.Writer) error {
	settings, err := spool.LoadRelayInviteSettings()
	if err != nil {
		return err
	}
	invite, id, err := spool.NewCommonsInvite()
	if err != nil {
		return err
	}
	secret, _, err := spool.DecodeCommonsInvite(invite)
	if err != nil {
		return err
	}
	var link, warning string
	if settings.PublicURL != "" {
		if link, warning, err = spool.MintRelayInvite(settings, secret); err != nil {
			return err
		}
	}
	fmt.Fprintf(stdout, "invite: %s\n", invite)
	if link != "" {
		fmt.Fprintf(stdout, "link: %s\n", link)
	}
	fmt.Fprintf(stdout, "SPOOL_COMMONS_ID=%s\n", spool.HexID(id[:]))
	switch {
	case link == "":
		fmt.Fprintln(stderr, "note: set SPOOL_PUBLIC_URL (wss://host/spool/v1) to also print a relay invite link")
	default:
		fmt.Fprintln(stderr, "warning:", linkWarning)
	}
	if warning != "" {
		fmt.Fprintln(stderr, "warning:", warning)
	}
	return nil
}

// relayLinkFromStdin mints a relay invite link for an existing room from its knit-commons:v1 invite,
// read from standard input so the secret stays out of arguments and shell history.
func relayLinkFromStdin(stdin io.Reader, stdout, stderr io.Writer) error {
	settings, err := spool.LoadRelayInviteSettings()
	if err != nil {
		return err
	}
	if settings.PublicURL == "" {
		return errors.New("--link-from-stdin needs SPOOL_PUBLIC_URL (wss://host/spool/v1)")
	}
	line, err := bufio.NewReader(io.LimitReader(stdin, 4096)).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	secret, id, err := spool.DecodeCommonsInvite(strings.TrimSpace(line))
	if err != nil {
		return fmt.Errorf("standard input: %w", err)
	}
	link, warning, err := spool.MintRelayInvite(settings, secret)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "link: %s\nSPOOL_COMMONS_ID=%s\n", link, spool.HexID(id[:]))
	fmt.Fprintln(stderr, "warning:", linkWarning)
	if warning != "" {
		fmt.Fprintln(stderr, "warning:", warning)
	}
	return nil
}
