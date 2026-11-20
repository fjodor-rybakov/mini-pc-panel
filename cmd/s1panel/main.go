// Command s1panel is the Go rewrite of mini-pc-panel: a Linux background
// daemon driving the AceMagic S1 LCD bezel panel and its LED strip, plus a
// built-in browser editor for telemetry widgets.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/fjodor-rybakov/mini-pc-panel/internal/app"
	"github.com/fjodor-rybakov/mini-pc-panel/internal/config"
	"github.com/fjodor-rybakov/mini-pc-panel/internal/display"
	"github.com/fjodor-rybakov/mini-pc-panel/internal/server"
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

	var httpSrv *server.Server
	if admin != "" {
		httpSrv = server.New(a, admin)
		go func() {
			log.Printf("editor on http://%s", editorURL(admin))
			err := httpSrv.ListenAndServe()
			if err != nil && !errors.Is(err, http.ErrServerClosed) && ctx.Err() == nil {
				log.Printf("server: %v", err)
			}
		}()
	}

	if err := a.Run(ctx); err != nil {
		log.Fatal(err)
	}
	// Stop accepting editor requests before closing the panel: an
	// in-flight /api/preview or /api/theme must finish (or fail) before
	// the HID/serial handles go away.
	if httpSrv != nil {
		sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := httpSrv.Shutdown(sctx); err != nil {
			log.Printf("server shutdown: %v", err)
		}
	}
	display.Shutdown() // hid.Exit() on Linux, no-op elsewhere
	log.Printf("bye")
}

// editorURL renders the listen address as a clickable URL: ":8686" needs
// a host; an explicit bind like "0.0.0.0:8686" is shown as-is (the old
// "http://localhost%s" format produced "http://localhost0.0.0.0:8686").
func editorURL(addr string) string {
	if strings.HasPrefix(addr, ":") {
		return "localhost" + addr
	}
	return addr
}
