package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	spool "github.com/plaonn/knit-spool-go"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	command := "run"
	if len(args) > 0 {
		command = args[0]
		args = args[1:]
	}
	if len(args) != 0 {
		return errors.New("usage: knit-spool [run|check|commons-invite]")
	}
	switch command {
	case "check":
		cfg, err := spool.LoadConfig()
		if err != nil {
			return err
		}
		fmt.Println(cfg.String())
		return nil
	case "commons-invite":
		invite, id, err := spool.NewCommonsInvite()
		if err != nil {
			return err
		}
		fmt.Printf("invite: %s\nSPOOL_COMMONS_ID=%s\n", invite, spool.HexID(id[:]))
		return nil
	case "run":
	default:
		return fmt.Errorf("unknown command %q; usage: knit-spool [run|check|commons-invite]", command)
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
	log.Printf("starting knit-spool-go (%s)", cfg.String())
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
