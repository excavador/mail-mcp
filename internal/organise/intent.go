// Package organise changes where mail lives, and only ever as an intent that
// was previewed and then approved.
//
// The rule that shapes everything here: a write is never "the agent moved the
// message it just read". Text inside a message can instruct a model; it
// cannot make the model's intent pass through a preview the owner approves.
// So an intent is resolved against the cache into a fixed set of stable ids
// (preview), the owner approves that set (apply), and apply acts only on the
// intersection of that set and what still matches.
//
// There is no delete here and there will not be one: nothing in this package
// sends EXPUNGE, STORE \Deleted, DELETE or RENAME. The only writes are
// CREATE, UID MOVE and UID COPY.
package organise

import (
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/excavador/mail-mcp/internal/accounts"
)

// Actions. What each does depends on the provider; callers state the intent
// and the provider rules below decide whether it is allowed.
const (
	ActionMove  = "move"  // leave the source folder, land in the target
	ActionLabel = "label" // land in the target, stay in the source
)

// Kinds of preview (and of history records that execute one).
const (
	KindApply   = "apply"
	KindUndo    = "undo"
	KindReapply = "reapply"
)

// maxIntentMessages bounds one intent. Beyond it the intent is refused, not
// truncated: an approval must cover everything that will move.
const maxIntentMessages = 50000

// Criterion selects messages in one folder. Fields are ANDed; at least one of
// From, ListID, GitHubReason, SubjectContains or To is required, so an intent
// can never mean "everything in the folder".
type Criterion struct {
	Folder          string    `json:"folder"`
	From            string    `json:"from,omitempty"`
	To              string    `json:"to,omitempty"`
	SubjectContains string    `json:"subject_contains,omitempty"`
	ListID          string    `json:"list_id,omitempty"`
	GitHubReason    string    `json:"github_reason,omitempty"`
	Since           time.Time `json:"since,omitzero"`
	Before          time.Time `json:"before,omitzero"`
}

// Intent is what the owner is asked to approve.
type Intent struct {
	Account   string    `json:"account"`
	Criterion Criterion `json:"criterion"`
	Target    string    `json:"target"`
	Action    string    `json:"action"`
}

func refusef(format string, args ...any) error { return SafeError(fmt.Sprintf(format, args...)) }

// SafeError is an error whose text may be shown to the client as it is.
type SafeError string

func (e SafeError) Error() string { return string(e) }

// Refusals with fixed text.
var (
	ErrNoMatcher      = SafeError("criterion needs at least one of from, list_id, github_reason, subject_contains, to")
	ErrTooManyMatched = SafeError("too many messages match (limit 50000); narrow the criterion")
	ErrUIDValidity    = SafeError("a folder was reset on the server (UIDVALIDITY changed); refresh the cache and preview again")
	ErrNoMoveCap      = SafeError("the server does not advertise MOVE; refusing to move")
	ErrTargetMissing  = SafeError("target folder does not exist; create it with create_folder first")
	// errTargetMissingFmt takes the folder and the account name.
	errTargetMissingFmt = "target folder %s does not exist on %s; create it with create_folder first"
	ErrSourceMissing    = SafeError("source folder does not exist on the server")
	ErrExpired          = SafeError("preview expired or unknown; preview again")
	ErrBusy             = SafeError("another write is in progress")
	ErrSpecialUse       = SafeError("that folder is a special-use folder (Trash, Junk, Drafts, Sent, All Mail or Flagged); not allowed")
)

// Validate checks an intent's shape and the provider's rules for it.
func (in Intent) Validate(p accounts.Provider) error {
	c := in.Criterion
	if err := checkName(c.Folder); err != nil {
		return SafeError("source folder: " + err.Error())
	}
	if c.From == "" && c.ListID == "" && c.GitHubReason == "" && c.SubjectContains == "" && c.To == "" {
		return ErrNoMatcher
	}
	for _, s := range []string{c.From, c.To, c.SubjectContains, c.ListID, c.GitHubReason} {
		if len(s) > 256 || !utf8.ValidString(s) || strings.ContainsFunc(s, unicode.IsControl) {
			return SafeError("criterion values must be short, valid text without control characters")
		}
	}
	return in.CheckMove(p)
}

// CheckMove applies the provider rules to the folders and action of an
// intent. It is separate from Validate because undo builds an intent from an
// explicit set of ids and has no matcher.
func (in Intent) CheckMove(p accounts.Provider) error {
	if in.Action != ActionMove && in.Action != ActionLabel {
		return refusef("action must be %q or %q", ActionMove, ActionLabel)
	}
	if err := checkName(in.Target); err != nil {
		return SafeError("target: " + err.Error())
	}
	src, dst := in.Criterion.Folder, in.Target
	if src == dst {
		return SafeError("source and target are the same folder")
	}
	switch p {
	case accounts.Gmail:
		// A move out of All Mail would archive nothing and strip nothing;
		// refusing it is clearer than a no-op. (apply also refuses any folder
		// the server marks \All.) System folders are not targets for now.
		if in.Action == ActionMove && strings.EqualFold(src, "[Gmail]/All Mail") {
			return SafeError("cannot move out of [Gmail]/All Mail: it holds every message; use label, or pick the label or INBOX as source")
		}
		// Allowlist: INBOX or a label whose name does not start with "[".
		// That keeps out [Gmail]/... and its localised twins ([Google Mail]/...).
		if dst != "INBOX" && strings.HasPrefix(dst, "[") {
			return SafeError("targets under [Gmail]/ and other system folders starting with [ are not supported; use INBOX or a label")
		}
	case accounts.Proton:
		if strings.HasPrefix(dst, "[") {
			return SafeError("targets starting with [ are not supported")
		}
		if hasPrefixFold(dst, "All Mail") || hasPrefixFold(dst, "Spam") || hasPrefixFold(dst, "Trash") {
			return SafeError("All Mail, Spam and Trash are not valid targets")
		}
		folder := strings.HasPrefix(dst, "Folders/") || dst == "INBOX" || dst == "Archive"
		switch {
		case in.Action == ActionMove && !folder:
			return SafeError("on Proton, move needs a Folders/... target (or INBOX or Archive); use label for Labels/...")
		case in.Action == ActionLabel && !strings.HasPrefix(dst, "Labels/"):
			return SafeError("on Proton, label needs a Labels/... target; use move for Folders/...")
		}
	}
	return nil
}

func hasPrefixFold(s, prefix string) bool {
	return len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix)
}

const maxNameBytes = 200

// checkName is the part of a folder name that is the same everywhere: valid
// text, bounded, no wildcards (a LIST pattern must stay a literal name), no
// control characters, and a path that cannot climb or be empty.
func checkName(name string) error {
	switch {
	case name == "":
		return SafeError("name is empty")
	case len(name) > maxNameBytes:
		return refusef("name is longer than %d bytes", maxNameBytes)
	case !utf8.ValidString(name):
		return SafeError("name is not valid UTF-8")
	case strings.ContainsAny(name, "*%"):
		return SafeError("name may not contain * or %")
	case strings.ContainsFunc(name, func(r rune) bool { return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) }):
		return SafeError("name may not contain control characters")
	case strings.HasPrefix(name, "/") || strings.HasSuffix(name, "/"):
		return SafeError("name may not start or end with /")
	}
	for _, seg := range strings.Split(name, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return SafeError("name may not have empty, . or .. segments")
		}
	}
	return nil
}

// ValidateNewFolder checks a name for create_folder.
func ValidateNewFolder(p accounts.Provider, name string) error {
	if err := checkName(name); err != nil {
		return err
	}
	if strings.HasPrefix(name, "[") {
		return SafeError("names starting with [ are reserved for system folders")
	}
	switch p {
	case accounts.Proton:
		if !strings.HasPrefix(name, "Folders/") && !strings.HasPrefix(name, "Labels/") {
			return SafeError("on Proton a new folder must start with Folders/ or Labels/")
		}
	}
	return nil
}
