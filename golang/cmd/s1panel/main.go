// Command s1panel is the Go rewrite of mini-pc-panel: a Linux background
// daemon driving the AceMagic S1 LCD bezel panel and its LED strip, plus a
// built-in browser editor for telemetry widgets.
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/fjodor-rybakov/mini-pc-panel/golang/internal/app"
	"github.com/fjodor-rybakov/mini-pc-panel/golang/internal/config"
	"github.com/fjodor-rybakov/mini-pc-panel/golang/internal/display"
	"github.com/fjodor-rybakov/mini-pc-panel/golang/internal/server"
)

func main() {
	log.SetPrefix("s1panel: ")
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)

	configPath := flag.String("config", "config.json", "daemon configuration file")
	adminAddr := flag.String("admin", "", "editor HTTP listen address; defaults to the config's \"listen\" (pass -admin \"\" to disable)")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatal(err)
	}

	// The original config.json carried "listen" ("0.0.0.0:8686"); honour it
	// unless -admin was passed explicitly on the command line.
	admin := *adminAddr
	explicit := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "admin" {
			explicit = true
		}
	})
	if !explicit {
		admin = cfg.Listen
	}

	a, err := app.New(cfg)
	if err != nil {
		log.Fatal(err)
	}

	// graceful shutdown per spec §5: SIGINT/SIGTERM close USB descriptors
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if admin != "" {
		srv := server.New(a, admin)
		go func() {
			log.Printf("editor on http://localhost%s", admin)
			if err := srv.ListenAndServe(); err != nil && ctx.Err() == nil {
				log.Printf("server: %v", err)
			}
		}()
	}

	if err := a.Run(ctx); err != nil {
		log.Fatal(err)
	}
	display.Shutdown() // hid.Exit() on Linux, no-op elsewhere
	log.Printf("bye")
}
