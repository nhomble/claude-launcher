// Package nodes models the ring.
//
// Membership is STATIC and hand-maintained — no election, no discovery, no
// gossip. The leader holds the follower list (CLAUDE_LAUNCHER_FOLLOWERS); a
// follower knows only itself. Each node is the sole authority for its own
// (live, in-memory) sessions — the leader never stores a follower's state, it
// queries it on demand and forwards mutations. So there is nothing to keep
// consistent across nodes, hence nothing to coordinate beyond plain HTTP.
//
// The routing key is the node ID. URL is empty for self (handled in-process,
// never looped back over HTTP).
package nodes

import (
	"fmt"
	"runtime"
	"strings"

	"github.com/nhomble/claude-launcher/internal/config"
)

type Node struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	URL      string `json:"-"` // base URL of a follower, empty for self
	Self     bool   `json:"self"`
	Platform string `json:"platform,omitempty"` // self: this process; followers: via /healthz
}

type Ring struct {
	self      Node
	followers []Node
	role      config.Role
}

// New builds the ring from config. A malformed follower entry is a startup
// error — better than silently dropping a machine from the ring.
func New(cfg config.Config) (*Ring, error) {
	self := Node{
		ID:       cfg.NodeID,
		Label:    cfg.NodeLabel,
		Self:     true,
		Platform: runtime.GOOS,
	}
	if self.Label == "" {
		self.Label = self.ID
	}

	r := &Ring{self: self, role: cfg.Role}
	if cfg.Role != config.Leader || strings.TrimSpace(cfg.FollowersSpec) == "" {
		return r, nil
	}

	for _, entry := range strings.Split(cfg.FollowersSpec, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		parts := strings.Split(entry, "|")
		var id, label, url string
		if len(parts) > 0 {
			id = strings.TrimSpace(parts[0])
		}
		if len(parts) > 1 {
			label = strings.TrimSpace(parts[1])
		}
		if len(parts) > 2 {
			url = strings.TrimSpace(parts[2])
		}
		if id == "" || url == "" {
			return nil, fmt.Errorf("bad CLAUDE_LAUNCHER_FOLLOWERS entry %q (want id|label|url)", entry)
		}
		// A leader accidentally listing itself is a no-op, not a self-proxy loop.
		if id == self.ID {
			continue
		}
		if label == "" {
			label = id
		}
		r.followers = append(r.followers, Node{ID: id, Label: label, URL: strings.TrimRight(url, "/")})
	}
	return r, nil
}

func (r *Ring) Role() config.Role { return r.role }
func (r *Ring) Self() Node        { return r.self }
func (r *Ring) Followers() []Node { return r.followers }

// All returns self first, then followers. A follower returns just itself.
func (r *Ring) All() []Node {
	return append([]Node{r.self}, r.followers...)
}

// Get resolves a routing id to a node. Empty or self → self; unknown → false.
func (r *Ring) Get(id string) (Node, bool) {
	want := strings.TrimSpace(id)
	if want == "" || want == r.self.ID {
		return r.self, true
	}
	for _, n := range r.followers {
		if n.ID == want {
			return n, true
		}
	}
	return Node{}, false
}
