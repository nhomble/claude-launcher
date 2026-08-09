// Package server wires the HTTP API and the htmx UI.
//
// Two layers share one set of data helpers (see data.go):
//   - /api/*  — a plain JSON API. This is also what the ring speaks: a leader
//     proxies host-specific calls to a follower's /api/*.
//   - /ui/*   — HTML fragments for htmx. Always rendered by the node serving
//     the page; per-host data is fetched through the same helpers.
package server

import (
	"encoding/json"
	"errors"
	"html/template"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/nhomble/claude-launcher/internal/catalog"
	"github.com/nhomble/claude-launcher/internal/config"
	"github.com/nhomble/claude-launcher/internal/nodes"
	"github.com/nhomble/claude-launcher/internal/recents"
	"github.com/nhomble/claude-launcher/internal/sessions"
	"github.com/nhomble/claude-launcher/internal/shells"
	"github.com/nhomble/claude-launcher/web"
)

type Server struct {
	cfg      config.Config
	ring     *nodes.Ring
	sessions *sessions.Manager
	recents  *recents.Store
	// Only for the model dropdown — /api/models and the page's first paint.
	// Model resolution at launch time happens in the sessions manager, which
	// holds the same store.
	catalog *catalog.Store
	shells  *shells.Registry
	tpl     *template.Template
}

func New(cfg config.Config, ring *nodes.Ring, mgr *sessions.Manager, store *catalog.Store, reg *shells.Registry) (*Server, error) {
	tpl, err := template.New("").Funcs(template.FuncMap{
		"uptime": uptime,
	}).ParseFS(web.Templates, "templates/*.html")
	if err != nil {
		return nil, err
	}
	return &Server{
		cfg:      cfg,
		ring:     ring,
		sessions: mgr,
		recents:  recents.New(cfg.DataDir),
		catalog:  store,
		shells:   reg,
		tpl:      tpl,
	}, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", s.healthz)

	// ── JSON API ───────────────────────────────────────────────────────────
	mux.HandleFunc("GET /api/nodes", s.apiNodes)
	mux.HandleFunc("GET /api/models", s.apiModels)
	mux.HandleFunc("GET /api/shells", s.routed(s.apiShells))
	mux.HandleFunc("GET /api/config", s.routed(s.apiConfig))
	mux.HandleFunc("GET /api/browse", s.routed(s.apiBrowse))
	mux.HandleFunc("POST /api/mkdir", s.routed(s.apiMkdir))
	mux.HandleFunc("GET /api/recents", s.routed(s.apiRecents))
	mux.HandleFunc("GET /api/sessions", s.apiSessions)
	mux.HandleFunc("POST /api/sessions", s.routed(s.apiSpawn))
	mux.HandleFunc("DELETE /api/sessions/{id}", s.routed(s.apiKill))
	mux.HandleFunc("GET /api/sessions/{id}/log", s.routed(s.apiLog))

	// ── htmx fragments ─────────────────────────────────────────────────────
	mux.HandleFunc("GET /ui/sessions", s.uiSessions)
	mux.HandleFunc("POST /ui/sessions", s.uiLaunch)
	mux.HandleFunc("DELETE /ui/sessions/{id}", s.uiKill)
	mux.HandleFunc("GET /ui/host", s.uiHost)
	mux.HandleFunc("GET /ui/browse", s.uiBrowse)
	mux.HandleFunc("POST /ui/mkdir", s.uiMkdir)

	// ── UI ─────────────────────────────────────────────────────────────────
	static, _ := fs.Sub(web.Static, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(static))))
	mux.HandleFunc("GET /{$}", s.index)

	return mux
}

// routed resolves the `?node=` target: self is handled locally, a follower is
// proxied, an unknown id is a 404.
func (s *Server) routed(h func(http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("node")
		n, ok := s.ring.Get(id)
		if !ok {
			writeJSON(w, http.StatusNotFound, errBody("unknown node: "+id))
			return
		}
		if !n.Self {
			proxy(w, r, n)
			return
		}
		h(w, r)
	}
}

// ── JSON API ─────────────────────────────────────────────────────────────────

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"role":     string(s.ring.Role()),
		"nodeId":   s.ring.Self().ID,
		"platform": s.ring.Self().Platform,
		"sessions": s.sessions.Count(),
		"shells":   s.shells.IDs(),
	})
}

func (s *Server) apiNodes(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.probeRing())
}

func (s *Server) apiModels(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.catalog.Models())
}

func (s *Server) apiShells(w http.ResponseWriter, r *http.Request) {
	out, _ := s.shellsFor(s.ring.Self())
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) apiConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"defaultDir": s.cfg.DefaultDir})
}

func (s *Server) apiRecents(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.recents.List())
}

func (s *Server) apiBrowse(w http.ResponseWriter, r *http.Request) {
	res, _ := s.browseFor(s.ring.Self(), r.URL.Query().Get("path"))
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) apiMkdir(w http.ResponseWriter, r *http.Request) {
	var body struct{ Path, Name string }
	if err := decodeJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("invalid JSON body"))
		return
	}
	path, err := s.mkdirOn(s.ring.Self(), body.Path, body.Name)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody(err.Error()))
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"path": path})
}

func (s *Server) apiSessions(w http.ResponseWriter, r *http.Request) {
	rows := s.allSessions(r.Header.Get(FanoutHeader) != "")
	if rows == nil {
		rows = []sessions.Session{}
	}
	writeJSON(w, http.StatusOK, rows)
}

func (s *Server) apiSpawn(w http.ResponseWriter, r *http.Request) {
	var opts sessions.SpawnOpts
	if err := decodeJSON(r, &opts); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("invalid JSON body"))
		return
	}
	sess, err := s.spawnOn(s.ring.Self(), opts)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody(err.Error()))
		return
	}
	writeJSON(w, http.StatusCreated, sess)
}

func (s *Server) apiKill(w http.ResponseWriter, r *http.Request) {
	if err := s.removeOn(s.ring.Self(), r.PathValue("id")); err != nil {
		writeJSON(w, http.StatusNotFound, errBody("not found"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) apiLog(w http.ResponseWriter, r *http.Request) {
	log, ok := s.sessions.TailLog(r.PathValue("id"))
	if !ok {
		writeJSON(w, http.StatusNotFound, errBody("not found"))
		return
	}
	w.Header().Set("content-type", "text/plain; charset=utf-8")
	w.Write([]byte(log))
}

// ── htmx fragments ───────────────────────────────────────────────────────────

// pageData is everything the full page needs on first paint. Fragments reuse
// the pieces they replace.
type pageData struct {
	Nodes      []nodeView
	MultiNode  bool
	Node       string // currently targeted node id
	Models     []catalog.Model
	Shells     []shellView
	Recents    []string
	DefaultDir string
	Sessions   []sessions.Session
	Err        string
}

// target resolves the node the UI form is pointed at, defaulting to self.
func (s *Server) target(r *http.Request) (nodes.Node, error) {
	id := r.URL.Query().Get("node")
	if id == "" {
		id = r.FormValue("node")
	}
	n, ok := s.ring.Get(id)
	if !ok {
		return nodes.Node{}, errors.New("unknown node: " + id)
	}
	return n, nil
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	ring := s.probeRing()
	self := s.ring.Self()

	data := pageData{
		Nodes:     ring,
		MultiNode: len(ring) > 1,
		Node:      self.ID,
		Models:    s.catalog.Models(),
		Sessions:  s.allSessions(false),
	}
	data.Shells, _ = s.shellsFor(self)
	data.Recents, _ = s.recentsFor(self)
	data.DefaultDir, _ = s.defaultDirFor(self)

	s.render(w, "index.html", data)
}

// uiSessions is the polled table fragment (the ring-wide aggregate).
func (s *Server) uiSessions(w http.ResponseWriter, r *http.Request) {
	s.render(w, "sessions", pageData{
		Sessions:  s.allSessions(false),
		MultiNode: len(s.ring.All()) > 1,
	})
}

// uiLaunch spawns on the targeted node and returns the refreshed table, with
// the error line and the recents list swapped out-of-band.
func (s *Server) uiLaunch(w http.ResponseWriter, r *http.Request) {
	data := pageData{MultiNode: len(s.ring.All()) > 1}

	n, err := s.target(r)
	if err == nil {
		_, err = s.spawnOn(n, sessions.SpawnOpts{
			Dir:   r.FormValue("dir"),
			Shell: r.FormValue("shell"),
			Name:  r.FormValue("name"),
			Model: r.FormValue("model"),
		})
	}
	if err != nil {
		data.Err = err.Error()
	} else {
		data.Recents, _ = s.recentsFor(n)
		data.Node = n.ID
	}
	data.Sessions = s.allSessions(false)

	// A failed launch must not blow away the recents datalist, so only send the
	// OOB recents swap when we actually have a fresh list.
	if data.Err != "" {
		s.render(w, "sessions-with-error", data)
		return
	}
	s.render(w, "sessions-after-launch", data)
}

func (s *Server) uiKill(w http.ResponseWriter, r *http.Request) {
	// The row carries the owning node — never the form's currently selected one.
	n, ok := s.ring.Get(r.URL.Query().Get("node"))
	data := pageData{MultiNode: len(s.ring.All()) > 1}
	if !ok {
		data.Err = "unknown node: " + r.URL.Query().Get("node")
	} else if err := s.removeOn(n, r.PathValue("id")); err != nil {
		data.Err = err.Error()
	}
	data.Sessions = s.allSessions(false)
	s.render(w, "sessions-with-error", data)
}

// uiHost re-sources everything that is per-machine after a host switch.
func (s *Server) uiHost(w http.ResponseWriter, r *http.Request) {
	n, err := s.target(r)
	if err != nil {
		s.render(w, "host-panel", pageData{Err: err.Error()})
		return
	}
	data := pageData{Node: n.ID, MultiNode: len(s.ring.All()) > 1}
	data.Shells, err = s.shellsFor(n)
	if err != nil {
		data.Err = "node " + n.ID + " unreachable: " + err.Error()
	}
	data.Recents, _ = s.recentsFor(n)
	data.DefaultDir, _ = s.defaultDirFor(n)
	s.render(w, "host-panel", data)
}

type browseData struct {
	Node   string
	Path   string
	Parent string
	Home   string
	Drives []string
	Dirs   []browseEntry
	Error  string
	MkDir  bool // render the inline "new folder" form
}

type browseEntry struct{ Name, Path string }

func (s *Server) uiBrowse(w http.ResponseWriter, r *http.Request) {
	n, err := s.target(r)
	if err != nil {
		s.render(w, "browser", browseData{Error: err.Error()})
		return
	}
	res, err := s.browseFor(n, r.URL.Query().Get("path"))
	d := browseData{Node: n.ID, MkDir: r.URL.Query().Get("mkdir") == "1"}
	if err != nil {
		d.Error = err.Error()
		s.render(w, "browser", d)
		return
	}
	d.Path, d.Parent, d.Home, d.Drives, d.Error = res.Path, res.Parent, res.Home, res.Drives, res.Error
	for _, e := range res.Entries {
		d.Dirs = append(d.Dirs, browseEntry{Name: e.Name, Path: e.Path})
	}
	s.render(w, "browser", d)
}

// uiMkdir creates a subfolder under the directory currently shown, then
// re-renders the browser inside it.
func (s *Server) uiMkdir(w http.ResponseWriter, r *http.Request) {
	n, err := s.target(r)
	if err != nil {
		s.render(w, "browser", browseData{Error: err.Error()})
		return
	}
	parent := r.FormValue("path")
	path, err := s.mkdirOn(n, parent, r.FormValue("foldername"))
	if err != nil {
		res, _ := s.browseFor(n, parent)
		d := browseData{Node: n.ID, Path: res.Path, Parent: res.Parent, Home: res.Home, Drives: res.Drives, Error: err.Error(), MkDir: true}
		for _, e := range res.Entries {
			d.Dirs = append(d.Dirs, browseEntry{Name: e.Name, Path: e.Path})
		}
		s.render(w, "browser", d)
		return
	}
	res, _ := s.browseFor(n, path)
	d := browseData{Node: n.ID, Path: res.Path, Parent: res.Parent, Home: res.Home, Drives: res.Drives, Error: res.Error}
	for _, e := range res.Entries {
		d.Dirs = append(d.Dirs, browseEntry{Name: e.Name, Path: e.Path})
	}
	s.render(w, "browser", d)
}

// ── helpers ──────────────────────────────────────────────────────────────────

func (s *Server) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("content-type", "text/html; charset=utf-8")
	if err := s.tpl.ExecuteTemplate(w, name, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("content-type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func errBody(msg string) map[string]string { return map[string]string{"error": msg} }

func decodeJSON(r *http.Request, out any) error {
	return json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20)).Decode(out)
}

// uptime renders how long a session has been up, from its epoch-ms start.
func uptime(startedAt int64, status string) string {
	if status != "running" {
		return "stopped"
	}
	sec := int64(time.Since(time.UnixMilli(startedAt)).Seconds())
	if sec < 0 {
		sec = 0
	}
	switch {
	case sec < 60:
		return strconv.FormatInt(sec, 10) + "s"
	case sec < 3600:
		return strconv.FormatInt(sec/60, 10) + "m"
	case sec < 86400:
		return strconv.FormatInt(sec/3600, 10) + "h " + strconv.FormatInt((sec%3600)/60, 10) + "m"
	default:
		return strconv.FormatInt(sec/86400, 10) + "d"
	}
}

// Startup banner text, kept here so main stays thin.
func (s *Server) Banner(port int) string {
	var b strings.Builder
	b.WriteString("claude-launcher on http://0.0.0.0:" + strconv.Itoa(port))
	b.WriteString("  (http://" + s.cfg.Host + ":" + strconv.Itoa(port) + "/)\n")
	b.WriteString("  role: " + string(s.ring.Role()) + "  node: " + s.ring.Self().ID + "\n")
	det := strings.Join(s.shells.IDs(), ", ")
	if det == "" {
		det = "(NONE DETECTED)"
	}
	b.WriteString("  shells detected: " + det)
	if f := s.ring.Followers(); len(f) > 0 {
		parts := make([]string, 0, len(f))
		for _, n := range f {
			parts = append(parts, n.ID+"→"+n.URL)
		}
		b.WriteString("\n  followers: " + strings.Join(parts, ", "))
	}
	return b.String()
}
