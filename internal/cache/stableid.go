package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-message/textproto"

	"github.com/excavador/mail-mcp/internal/accounts"
)

var pmIDRE = regexp.MustCompile(`^[A-Za-z0-9_=-]{1,128}$`)

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
// Gmail: the key is X-GM-MSGID (IMAP extension X-GM-EXT-1), a 64-bit id
// Gmail assigns once per message regardless of how many labels carry it, so
// the same message under INBOX, a label and [Gmail]/All Mail has one id. It is
// used, as "gm:<decimal>", when the account is Gmail, the server advertises
// X-GM-EXT-1 and the fetch returned a non-zero value (see stableIDFor).
// go-imap v2.0.0-beta.8 could not fetch it; the excavador/go-imap fork
// (v2.0.0-beta.8.gmext.1, see go.mod) can.
//
// Without it Gmail falls back to the Message-ID header, disambiguated with the
// message's RFC822.SIZE and INTERNALDATE: Message-ID alone is not unique
// (senders reuse and omit it), but a different message with the same
// Message-ID, the same size and the same arrival second is, for a mailbox,
// the same message. Gmail reports identical size and internal date for a
// message under every label, so [Gmail]/All Mail and a label folder agree.
// That scheme is a heuristic where X-GM-MSGID is exact. Ids are namespaced by
// scheme ("gm:", "mid:") and the schemes never collide; a mailbox cached
// under "mid:" and later refreshed under "gm:" is indexed again in full.
//
// Any provider that lacks its preferred key (a Bridge version that does not
// stamp the header) uses the same Message-ID scheme rather than failing.
func stableID(p accounts.Provider, header []byte, size int64, internal time.Time) (string, error) {
	h, err := textproto.ReadHeader(bufioReader(header))
	if err != nil {
		return "", fmt.Errorf("parse headers: %w", err)
	}
	if p == accounts.Proton {
		// A message's own headers are attacker-influenced unless Bridge
		// overwrites this one, so it is trusted only when it occurs exactly
		// once and looks like an id; anything else falls through to the
		// Message-ID scheme. Whether Bridge strips an inbound X-Pm-Internal-Id
		// must be verified against a live Bridge.
		if vs := h.Values("X-Pm-Internal-Id"); len(vs) == 1 {
			if v := strings.TrimSpace(vs[0]); pmIDRE.MatchString(v) {
				return "pm:" + v, nil
			}
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

// stableIDFor is stableID with the Gmail extension in front: when the account
// is Gmail and gmMsgID (X-GM-MSGID, 0 when not fetched or not supported) is
// non-zero, the id is "gm:<decimal>". Anything else is stableID.
func stableIDFor(p accounts.Provider, header []byte, size int64, internal time.Time, gmMsgID uint64) (string, error) {
	if p == accounts.Gmail && gmMsgID != 0 {
		return "gm:" + strconv.FormatUint(gmMsgID, 10), nil
	}
	return stableID(p, header, size, internal)
}
