package sessions

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

// newUUID returns a random (version 4, variant 10) UUID in the canonical
// 8-4-4-4-12 hex form.
//
// Session ids are UUIDs because they are handed to `claude --session-id`: one
// id is simultaneously the launcher's map key, the URL path key, the log
// filename and claude's own transcript/session id (the `--resume` target).
// 122 bits of entropy means an id can never collide, and because a lookup
// resolves to the *live struct that owns the process handle directly — never
// through an OS pid — a recycled pid can never make an old id resolve to an
// unrelated process.
func newUUID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failing is effectively fatal, but a session id is not
		// worth panicking over: fall back to a time-seeded value that still
		// has the right shape.
		n := uint64(time.Now().UnixNano())
		for i := range b {
			b[i] = byte(n >> (8 * (i % 8)))
		}
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	h := hex.EncodeToString(b)
	return fmt.Sprintf("%s-%s-%s-%s-%s", h[0:8], h[8:12], h[12:16], h[16:20], h[20:32])
}
