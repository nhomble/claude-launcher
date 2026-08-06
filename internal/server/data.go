package server

import (
	"fmt"
	"net/url"
	"sync"

	"github.com/nhomble/claude-launcher/internal/browse"
	"github.com/nhomble/claude-launcher/internal/nodes"
	"github.com/nhomble/claude-launcher/internal/sessions"
	"github.com/nhomble/claude-launcher/internal/shells"
)

// Node-aware data access. Every per-host read/mutation goes through one of
// these: self is answered in-process, a follower over its JSON API. The UI
// layer is always rendered by the node you loaded the page from.

type shellView struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

func (s *Server) shellsFor(n nodes.Node) ([]shellView, error) {
	if n.Self {
		out := make([]shellView, 0, len(shells.Available()))
		for _, sh := range shells.Available() {
			out = append(out, shellView{ID: sh.ID, Label: sh.Label})
		}
		return out, nil
	}
	var out []shellView
	err := getJSON(n, "/api/shells", nil, &out, proxyTimeout)
	return out, err
}

func (s *Server) recentsFor(n nodes.Node) ([]string, error) {
	if n.Self {
		return s.recents.List(), nil
	}
	var out []string
	err := getJSON(n, "/api/recents", nil, &out, proxyTimeout)
	return out, err
}

func (s *Server) defaultDirFor(n nodes.Node) (string, error) {
	if n.Self {
		return s.cfg.DefaultDir, nil
	}
	var out struct {
		DefaultDir string `json:"defaultDir"`
	}
	err := getJSON(n, "/api/config", nil, &out, proxyTimeout)
	return out.DefaultDir, err
}

func (s *Server) browseFor(n nodes.Node, path string) (browse.Result, error) {
	if n.Self {
		return browse.Browse(path), nil
	}
	q := url.Values{}
	if path != "" {
		q.Set("path", path)
	}
	var out browse.Result
	err := getJSON(n, "/api/browse", q, &out, proxyTimeout)
	return out, err
}

func (s *Server) mkdirOn(n nodes.Node, parent, name string) (string, error) {
	if n.Self {
		return browse.MakeDir(parent, name)
	}
	in := map[string]string{"path": parent, "name": name}
	var out struct {
		Path string `json:"path"`
	}
	err := sendJSON(n, "POST", "/api/mkdir", nil, in, &out)
	return out.Path, err
}

func (s *Server) spawnOn(n nodes.Node, opts sessions.SpawnOpts) (sessions.Session, error) {
	if n.Self {
		sess, err := s.sessions.Spawn(opts)
		if err == nil {
			s.recents.Add(sess.Dir)
		}
		return sess, err
	}
	var out sessions.Session
	err := sendJSON(n, "POST", "/api/sessions", nil, opts, &out)
	return out, err
}

func (s *Server) removeOn(n nodes.Node, id string) error {
	if n.Self {
		if !s.sessions.Remove(id) {
			return fmt.Errorf("not found")
		}
		return nil
	}
	return sendJSON(n, "DELETE", "/api/sessions/"+url.PathEscape(id), nil, nil, nil)
}

// allSessions is the one AGGREGATE read: the union of self plus every online
// follower, each row tagged with its owning node. Offline followers are simply
// omitted — never cached, never stalls the table.
func (s *Server) allSessions() []sessions.Session {
	all := s.ring.All()
	rows := make([][]sessions.Session, len(all))

	var wg sync.WaitGroup
	for i, n := range all {
		if n.Self {
			local := s.sessions.List()
			for j := range local {
				local[j].Node = n.ID
				local[j].NodeLabel = n.Label
			}
			rows[i] = local
			continue
		}
		wg.Add(1)
		go func(i int, n nodes.Node) {
			defer wg.Done()
			var remote []sessions.Session
			if err := getJSON(n, "/api/sessions", nil, &remote, fanoutTimeout); err != nil {
				return
			}
			for j := range remote {
				remote[j].Node = n.ID
				remote[j].NodeLabel = n.Label
			}
			rows[i] = remote
		}(i, n)
	}
	wg.Wait()

	var out []sessions.Session
	for _, r := range rows {
		out = append(out, r...)
	}
	// Newest first, matching each node's own ordering.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].StartedAt > out[j-1].StartedAt; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

type nodeView struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	Platform string `json:"platform,omitempty"`
	Self     bool   `json:"self"`
	Online   bool   `json:"online"`
	Role     string `json:"role,omitempty"`
}

// probeRing reports the ring as this node sees it: self (always online) plus
// each follower, probed live over /healthz.
func (s *Server) probeRing() []nodeView {
	all := s.ring.All()
	out := make([]nodeView, len(all))

	var wg sync.WaitGroup
	for i, n := range all {
		if n.Self {
			out[i] = nodeView{ID: n.ID, Label: n.Label, Platform: n.Platform, Self: true, Online: true, Role: string(s.ring.Role())}
			continue
		}
		out[i] = nodeView{ID: n.ID, Label: n.Label}
		wg.Add(1)
		go func(i int, n nodes.Node) {
			defer wg.Done()
			var h struct {
				Platform string `json:"platform"`
				Role     string `json:"role"`
			}
			if err := getJSON(n, "/healthz", nil, &h, probeTimeout); err != nil {
				return
			}
			out[i].Online = true
			out[i].Platform = h.Platform
			out[i].Role = h.Role
		}(i, n)
	}
	wg.Wait()
	return out
}
