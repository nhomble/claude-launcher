package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/nhomble/claude-launcher/internal/nodes"
)

// Follower calls are short-lived on purpose: one slow or dead node must never
// hang the leader's UI, it should surface as a clean error instead.
//
// fanoutTimeout and probeTimeout in particular must stay under the UI's
// 4s poll interval (web/templates/index.html, hx-trigger="every 4s"): a dead
// follower is probed on every poll, in parallel across followers but
// serially across polls, so a timeout >= the poll interval lets requests
// pile up against it indefinitely.
const (
	proxyTimeout  = 5 * time.Second
	probeTimeout  = 1500 * time.Millisecond
	fanoutTimeout = 3 * time.Second

	// maxTurnWait caps ?wait= on the turn long-poll. Kept well under any
	// sensible reverse-proxy idle timeout, and proxied calls get
	// proxyTimeout + wait so a long poll survives a hop to its owning node.
	maxTurnWait = 60 * time.Second
)

// FanoutHeader marks a request as one node querying another during a
// /api/sessions fan-out. A node that receives it answers with its OWN sessions
// only, never fanning out again — otherwise two nodes that each list the other
// as a follower would query each other forever.
const FanoutHeader = "X-Claude-Launcher-Fanout"

var client = &http.Client{Timeout: proxyTimeout}

// proxyClient returns a client bounded by timeout. Per-call rather than shared
// because the turn long-poll legitimately needs far longer than proxyTimeout,
// and one global client cannot serve both.
func proxyClient(timeout time.Duration) *http.Client { return &http.Client{Timeout: timeout} }

// proxy forwards the current request to a follower and copies its response back
// verbatim. Routing lives entirely in the `?node=` query param: we strip it
// before forwarding so the follower handles the request as its own local one
// (its node defaults to self). The body is passed through untouched.
//
// A transport failure becomes a 502 JSON error rather than a stall.
func proxy(w http.ResponseWriter, r *http.Request, n nodes.Node, timeout time.Duration) {
	target, err := url.Parse(n.URL)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, errBody(fmt.Sprintf("node %q has a bad URL: %v", n.ID, err)))
		return
	}
	target.Path = r.URL.Path
	q := r.URL.Query()
	q.Del("node")
	target.RawQuery = q.Encode()

	var body io.Reader
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		const maxProxyBody = 1 << 20
		// Read one byte past the limit so a body that's exactly at the edge
		// vs. one that overflows it are distinguishable — silently forwarding
		// a truncated body used to produce a confusing "invalid JSON" 400
		// from the follower instead of a clear error from the leader.
		b, err := io.ReadAll(io.LimitReader(r.Body, maxProxyBody+1))
		if err != nil {
			writeJSON(w, http.StatusBadGateway, errBody("reading request body: "+err.Error()))
			return
		}
		if len(b) > maxProxyBody {
			writeJSON(w, http.StatusRequestEntityTooLarge, errBody(fmt.Sprintf("request body exceeds %d bytes", maxProxyBody)))
			return
		}
		body = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(r.Context(), r.Method, target.String(), body)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, errBody(err.Error()))
		return
	}
	if ct := r.Header.Get("content-type"); ct != "" {
		req.Header.Set("content-type", ct)
	} else {
		req.Header.Set("content-type", "application/json")
	}

	resp, err := proxyClient(timeout).Do(req)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, errBody(fmt.Sprintf("node %q unreachable: %v", n.ID, err)))
		return
	}
	defer resp.Body.Close()

	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// getJSON fetches path from a follower and decodes it into out. headers are
// applied to the outgoing request (see FanoutHeader).
func getJSON(n nodes.Node, path string, query url.Values, out any, timeout time.Duration, headers ...[2]string) error {
	u, err := url.Parse(n.URL)
	if err != nil {
		return err
	}
	u.Path = path
	u.RawQuery = query.Encode()

	req, err := http.NewRequest(http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	for _, h := range headers {
		req.Header.Set(h[0], h[1])
	}

	c := &http.Client{Timeout: timeout}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return decodeRemoteError(resp)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// sendJSON performs a body-carrying request (POST/DELETE) against a follower.
func sendJSON(n nodes.Node, method, path string, query url.Values, in, out any) error {
	u, err := url.Parse(n.URL)
	if err != nil {
		return err
	}
	u.Path = path
	u.RawQuery = query.Encode()

	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, u.String(), body)
	if err != nil {
		return err
	}
	req.Header.Set("content-type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return decodeRemoteError(resp)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// decodeRemoteError turns a follower's {"error": …} body into a Go error, so
// the reason shows up in the UI instead of a bare status code.
func decodeRemoteError(resp *http.Response) error {
	var e struct {
		Error string `json:"error"`
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	if json.Unmarshal(b, &e) == nil && e.Error != "" {
		return fmt.Errorf("%s", e.Error)
	}
	return fmt.Errorf("remote returned HTTP %d", resp.StatusCode)
}
