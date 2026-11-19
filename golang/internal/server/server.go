// Package server serves the browser widget editor (PART B of the task):
// live telemetry for the editor canvas, "Примерить" which pushes the draft
// to the REAL panel, hot theme apply and manual LED control.
package server

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"path/filepath"
	"strconv"
	"time"

	"github.com/fjodor-rybakov/mini-pc-panel/golang/internal/config"
	"github.com/fjodor-rybakov/mini-pc-panel/golang/internal/metrics"
	"github.com/fjodor-rybakov/mini-pc-panel/golang/internal/render"
)

//go:embed web
var webFS embed.FS

// maxDraftBytes bounds a POSTed theme draft.
const maxDraftBytes = 1 << 20 // 1 MiB

// Panel is the subset of the daemon (*app.App) the editor talks to; keeping
// it an interface lets the HTTP layer be unit-tested without HID hardware.
type Panel interface {
	SensorNames() []string
	LatestReadings() map[string]metrics.Reading
	Theme() *config.Theme
	CurrentScreenIndex() int
	Config() *config.Config
	SwapTheme(t *config.Theme)
	PreviewFrame(t *config.Theme, screenIdx int) (*render.Frame, error)
	PreviewOnScreen(ctx context.Context, t *config.Theme, screenIdx int) (*render.Frame, error)
	ApplyLEDManual(s config.LEDSettings) error
	NextScreen()
	ShowScreen(id int) bool
	PauseRotation(on bool)
}

// Server bundles the editor routes; the panel reference gives access to the
// live display, theme and telemetry.
type Server struct {
	app  Panel
	addr string
	mux  *http.ServeMux
}

// New wires routes; the embedded web/ editor (vanilla JS, no build step)
// is served from "/". *app.App satisfies Panel.
func New(a Panel, addr string) *Server {
	s := &Server{app: a, addr: addr, mux: http.NewServeMux()}
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		panic("server: embed: " + err.Error())
	}
	s.mux.Handle("/", http.FileServer(http.FS(sub)))
	s.mux.HandleFunc("GET /api/state", s.handleState)
	s.mux.HandleFunc("GET /api/poll", s.handlePoll)
	s.mux.HandleFunc("GET /api/theme", s.handleTheme)
	s.mux.HandleFunc("POST /api/preview", s.handlePreview)
	s.mux.HandleFunc("POST /api/apply", s.handleApply)
	s.mux.HandleFunc("POST /api/led", s.handleLED)
	s.mux.HandleFunc("GET /api/themes", s.handleThemes)
	s.mux.HandleFunc("POST /api/theme", s.handleThemeSwitch)
	s.mux.HandleFunc("POST /api/next_screen", s.handleNextScreen)
	s.mux.HandleFunc("POST /api/pause", s.handlePause)
	return s
}

// ListenAndServe runs the editor HTTP server (blocking).
func (s *Server) ListenAndServe() error {
	httpSrv := &http.Server{
		Addr:              s.addr,
		Handler:           s.mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	return httpSrv.ListenAndServe()
}

// Handler exposes the routes (used by server_test.go via httptest).
func (s *Server) Handler() http.Handler { return s.mux }

// ---- DTOs ----

type readingDTO struct {
	Value   float64   `json:"value"`
	Max     float64   `json:"max"`
	Text    []string  `json:"text"`
	History []float64 `json:"history,omitempty"`
}

func dto(r metrics.Reading) readingDTO {
	return readingDTO{Value: r.Value, Max: r.Max, Text: r.Text, History: r.History}
}

func readingsDTO(rd map[string]metrics.Reading) map[string]readingDTO {
	out := make(map[string]readingDTO, len(rd))
	for k, v := range rd {
		out[k] = dto(v)
	}
	return out
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("server: encode: %v", err)
	}
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

// readDraft decodes and validates a theme draft POST body.
func readDraft(r *http.Request) (*config.Theme, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxDraftBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if len(body) > maxDraftBytes {
		return nil, fmt.Errorf("draft too large (max 1 MiB)")
	}
	return config.ParseTheme(body)
}

// ---- handlers ----

// handleState answers everything the editor needs on load: configured
// sensor names (dropdown), current live readings (real numbers), the live
// theme (draft seed) and which screen the panel is showing.
func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"sensors":        s.app.SensorNames(),
		"readings":       readingsDTO(s.app.LatestReadings()),
		"theme":          s.app.Theme(),
		"current_screen": s.app.CurrentScreenIndex(),
		"config": map[string]any{
			"theme_file": s.app.Config().ThemeFile,
			"led":        s.app.Config().LED,
		},
	})
}

// handlePoll re-serves the latest readings (the daemon loop samples every
// poll interval, so the editor numbers are real system values).
func (s *Server) handlePoll(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"readings": readingsDTO(s.app.LatestReadings()),
	})
}

func (s *Server) handleTheme(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.app.Theme())
}

// handleThemes lists the config's theme_list plus the live theme file, so
// the editor can offer a theme picker (the original GUI fed its dropdown
// from the theme_list in GET /api/config).
func (s *Server) handleThemes(w http.ResponseWriter, r *http.Request) {
	cfg := s.app.Config()
	writeJSON(w, http.StatusOK, map[string]any{
		"themes":     cfg.ThemeList,
		"theme_file": cfg.ThemeFile,
	})
}

// handleThemeSwitch hot-loads another theme from theme_list by file path
// or name. The live config's theme_file is updated in memory only; the
// config.json itself is never rewritten by the editor (hot-apply
// semantics, same as /api/apply without ?save).
func (s *Server) handleThemeSwitch(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 4096))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	var req struct {
		File string `json:"file"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if req.File == "" && req.Name == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("theme switch needs a file or name"))
		return
	}
	cfg := s.app.Config()
	var ref *config.ThemeRef
	for i := range cfg.ThemeList {
		cand := &cfg.ThemeList[i]
		if (req.File != "" && filepath.Clean(cand.File) == filepath.Clean(req.File)) ||
			(req.Name != "" && cand.Name == req.Name) {
			ref = cand
			break
		}
	}
	if ref == nil {
		writeErr(w, http.StatusNotFound, fmt.Errorf("theme %q not in theme_list", req.File+req.Name))
		return
	}
	path := ref.File
	if !filepath.IsAbs(path) {
		path = filepath.Join(cfg.Dir, path)
	}
	t, err := config.LoadTheme(path)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	cfg.ThemeFile = path
	s.app.SwapTheme(t)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "name": ref.Name, "file": path})
}

// handleNextScreen advances the screen rotation immediately; a body
// {"id":N} jumps straight to the theme screen carrying that id (the
// original /api/next_screen callback honours request.id the same way).
func (s *Server) handleNextScreen(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 4096))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	targetID := -1
	if len(body) > 0 {
		var req struct {
			ID *int `json:"id"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		if req.ID != nil {
			targetID = *req.ID
		}
	}
	if targetID >= 0 {
		if !s.app.ShowScreen(targetID) {
			writeErr(w, http.StatusNotFound, fmt.Errorf("no enabled screen with id %d", targetID))
			return
		}
	} else {
		s.app.NextScreen()
	}
	idx := s.app.CurrentScreenIndex()
	th := s.app.Theme()
	id := -1
	if idx >= 0 && idx < len(th.Screens) {
		id = th.Screens[idx].ID
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "index": idx, "id": id})
}

// handlePause freezes/unfreezes automatic screen rotation (?on=true|false);
// the original set it implicitly while editing and on theme_save.
func (s *Server) handlePause(w http.ResponseWriter, r *http.Request) {
	on := true
	if v := r.URL.Query().Get("on"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		on = b
	}
	s.app.PauseRotation(on)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "paused": on})
}

// handlePreview validates the draft and pushes it to the physical panel
// (full redraw) plus the screen LED; it replies with the rendered PNG so
// the editor can also show what the panel now displays.
func (s *Server) handlePreview(w http.ResponseWriter, r *http.Request) {
	idx, err := screenParam(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	t, err := readDraft(r)
	if err != nil {
		writeErr(w, http.StatusUnprocessableEntity, err)
		return
	}
	frame, err := s.app.PreviewOnScreen(r.Context(), t, idx)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	if err := frame.WritePNG(w); err != nil {
		log.Printf("server: preview png: %v", err)
	}
}

// handleApply hot-swaps the live theme (the daemon forces a full redraw on
// the next poll); ?save=1 additionally writes the theme file.
func (s *Server) handleApply(w http.ResponseWriter, r *http.Request) {
	t, err := readDraft(r)
	if err != nil {
		writeErr(w, http.StatusUnprocessableEntity, err)
		return
	}
	s.app.SwapTheme(t)
	saved := false
	if r.URL.Query().Get("save") == "1" {
		if err := config.SaveTheme(s.app.Config().ThemeFile, t); err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		saved = true
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "saved": saved})
}

func (s *Server) handleLED(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 4096))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	var settings config.LEDSettings
	if err := json.Unmarshal(body, &settings); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := s.app.ApplyLEDManual(settings); err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func screenParam(r *http.Request) (int, error) {
	v := r.URL.Query().Get("screen")
	if v == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("bad screen parameter %q", v)
	}
	return n, nil
}
