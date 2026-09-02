// server.go is the local, read-only web panel described in SPEC.md Bölüm
// 7.1 — separate from report.go's single-file backtest report (Bölüm 7.3).
//
// Server wraps an already-open *store.Store: it issues nothing but SELECTs
// (via store's typed Get*/List* methods — see store.go's package doc
// comment, "the only package that speaks SQL") and never opens a second
// connection of its own, matching store.Open's single-connection
// discipline (SetMaxOpenConns(1)) documented there. Every HTTP route it
// registers is GET-only (net/http's ServeMux, since Go 1.22, answers any
// other method on a "GET /path" pattern with 405 automatically) — SPEC.md
// Bölüm 7.1's "Panel hiçbir yazma işlemi yapmaz" is enforced structurally
// here, not just by convention: there is no POST/PUT/DELETE handler
// anywhere in this package to accidentally wire up.
package web

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"time"

	"swingbot/internal/config"
	"swingbot/internal/store"
)

// staticFiles embeds every asset the panel's single-page shell needs
// (SPEC.md Bölüm 3 / Bölüm 7.1): the page shell, the panel's own CSS/JS,
// and the vendored (not CDN-loaded) TradingView Lightweight Charts build.
// Embedding keeps the binary a single deployable file even with the panel
// included.
//
//go:embed static/index.html static/app.js static/app.css static/vendor/lightweight-charts.standalone.js
var staticFiles embed.FS

// Server is the panel's HTTP handler factory. It holds no mutable state
// beyond what NewServer is given: st is read-only from this package's
// point of view, cfg is read once per request (never mutated), and
// startedAt only ever feeds /api/health's uptime figure.
type Server struct {
	st        *store.Store
	cfg       *config.Config
	version   string
	startedAt time.Time
}

// NewServer builds a Server around an already-open store and a validated
// config.Config. version is surfaced as-is by /api/health (callers
// typically pass their CLI's version string); it may be empty.
func NewServer(st *store.Store, cfg *config.Config, version string) *Server {
	return &Server{st: st, cfg: cfg, version: version, startedAt: time.Now().UTC()}
}

// Handler builds the full panel http.Handler: the JSON API (api.go) plus
// the embedded static assets, wrapped in a panic-recovery layer so a bug
// in one handler returns a JSON 500 instead of killing the whole process
// (this is a long-running local server, not a one-shot CLI command).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	s.registerAPI(mux)
	s.registerStatic(mux)
	return recoverMiddleware(mux)
}

// registerStatic wires the SPA shell (SPEC.md Bölüm 7.1's page table) and
// its own assets. Every page path serves the same index.html; app.js does
// client-side routing off window.location.pathname so a full reload on
// e.g. /runs/abc123 still renders the right view. Asset paths are
// registered individually (not a catch-all file server) so the route table
// here matches SPEC.md Bölüm 3's static/ listing exactly and an unknown
// path 404s instead of silently falling through to the file server.
func (s *Server) registerStatic(mux *http.ServeMux) {
	sub, err := fs.Sub(staticFiles, "static")
	if err != nil {
		// Can only happen if the //go:embed directive above and this Sub
		// call disagree about the embedded tree's shape — a build-time
		// programming error, not a runtime condition callers can recover
		// from.
		panic("web: embedded static assets missing: " + err.Error())
	}
	fileServer := http.FileServer(http.FS(sub))

	index := func(w http.ResponseWriter, r *http.Request) {
		serveIndex(w, sub)
	}

	// SPEC.md Bölüm 7.1's page table.
	mux.HandleFunc("GET /{$}", index)
	mux.HandleFunc("GET /positions", index)
	mux.HandleFunc("GET /proposals", index)
	mux.HandleFunc("GET /trades", index)
	mux.HandleFunc("GET /runs", index)
	mux.HandleFunc("GET /runs/{id}", index)
	mux.HandleFunc("GET /universe", index)

	// SPEC.md Bölüm 3's static/ listing, served verbatim from the embed.
	mux.Handle("GET /app.js", fileServer)
	mux.Handle("GET /app.css", fileServer)
	mux.Handle("GET /vendor/lightweight-charts.standalone.js", fileServer)
}

func serveIndex(w http.ResponseWriter, sub fs.FS) {
	b, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		http.Error(w, "panel varlıkları eksik (embed sorunu)", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(b)
}

func recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				writeError(w, http.StatusInternalServerError, fmt.Sprintf("panel: beklenmeyen hata: %v", rec))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// ListenAndServe binds addr and serves the panel until ctx is cancelled,
// at which point it shuts down gracefully (existing requests get up to 5s
// to finish). It returns nil on a clean shutdown, or the listener/serve
// error otherwise (including http.ErrServerClosed, which callers should
// treat as success — matching net/http.Server.Shutdown's own contract).
func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	httpServer := &http.Server{
		Addr:              addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() { errCh <- httpServer.ListenAndServe() }()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return httpServer.Shutdown(shutdownCtx)
	}
}

// IsLoopbackAddr reports whether addr's host (a "host:port" string, or a
// bare host) is a loopback address (127.0.0.1, ::1, or literally
// "localhost"). SPEC.md Bölüm 7.1 / Bölüm 13: the panel has zero
// authentication, so anything that is not loopback is reachable by
// whoever else can reach this machine. internal/config's Validate already
// warns specifically on "0.0.0.0"/"::"/"" (isAllInterfaces, Bölüm 8); this
// catches the gap that check does not — a LAN IP like "192.168.1.5:8080"
// is neither a wildcard host nor loopback, and is just as much a
// foot-gun. `swingbot serve` uses this to warn before binding.
func IsLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
