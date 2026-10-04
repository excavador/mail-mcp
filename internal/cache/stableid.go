package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-message/textproto"

	"github.com/excavador/mail-mcp/internal/accounts"
)

// idHeaderFields are the header fields the cheap stable-id fetch asks for.
// Message-ID is always wanted, as the fallback for every provider; Date,
// Subject and From make the last-resort key (no Message-ID) less collision-prone.
var idHeaderFields = []string{"X-Pm-Internal-Id", "Message-Id", "Date", "Subject", "From"}

// stableID derives the key that survives a move between folders, from the
// header block of one message plus its RFC822.SIZE and INTERNALDATE.
//
// Proton (via Bridge): Bridge stamps every message it serves with
// X-Pm-Internal-Id, which is Proton's own message id and does not change when
// the message changes folder. It is the key.
//
// Gmail: the right key is X-GM-MSGID (IMAP extension X-GM-EXT-1), a 64-bit id
// Gmail assigns once per message regardless of how many labels carry it.
// go-imap v2.0.0-beta.8 cannot fetch it: FetchOptions has no field for it,
// the item list the client writes is fixed, and a server reply carrying an
// attribute the library does not know makes the fetch fail with "unsupported
// msg-att name". Using it would mean forking the library, which we do not do.
// So Gmail falls back to the Message-ID header, disambiguated with the
// message's RFC822.SIZE and INTERNALDATE: Message-ID alone is not unique
// (senders reuse and omit it), but a different message with the same
// Message-ID, the same size and the same arrival second is, for a mailbox,
// the same message. Gmail reports identical size and internal date for a
// message under every label, so [Gmail]/All Mail and a label folder agree.
// If go-imap gains X-GM-MSGID support, switch to it: it is exact where this
// is a heuristic. Changing the key later means one full re-index, because
// ids are namespaced by scheme and the two schemes never collide.
//
// Any provider that lacks its preferred key (a Bridge version that does not
// stamp the header) uses the same Message-ID scheme rather than failing.
func stableID(p accounts.Provider, header []byte, size int64, internal time.Time) (string, error) {
	h, err := textproto.ReadHeader(bufioReader(header))
	if err != nil {
		return "", fmt.Errorf("parse headers: %w", err)
	}
	if p == accounts.Proton {
		if v := strings.TrimSpace(h.Get("X-Pm-Internal-Id")); v != "" {
			return "pm:" + v, nil
		}
	}
	mid := strings.TrimSpace(h.Get("Message-Id"))
	if mid == "" {
		// No Message-ID at all: size and date alone would merge unrelated
		// messages, so mix in the whole fetched header block, which then
		// still identifies the same message wherever it is listed.
		mid = "nomid:" + string(header)
	}
	sum := sha256.Sum256([]byte(mid + "\x00" + strconv.FormatInt(size, 10) + "\x00" + strconv.FormatInt(internal.Unix(), 10)))
	return "mid:" + hex.EncodeToString(sum[:]), nil
}
