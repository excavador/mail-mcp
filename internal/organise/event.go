package organise

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// EventPreview is a calendar event awaiting approval: exactly what
// create_event will insert, and what the owner is shown about it. It lives in
// memory only, for PreviewTTL, and is kept apart from intents and drafts: an
// event token opens nothing else and nothing else opens it.
type EventPreview struct {
	Token    string
	Expires  time.Time
	Nonce    [16]byte
	question [16]byte // see Preview.question

	Account  string
	Calendar string
	Title    string
	Start    time.Time // in TimeZone
	End      time.Time
	TimeZone string
	// Attendees are bare lowercase addresses, deduplicated, in the order
	// given; AttendeesStr is the same comma-joined (what expect_attendees
	// restates).
	Attendees    []string
	AttendeesStr string
	Description  string
	Location     string
	Meet         bool
}

type eventCanonical struct {
	Kind        string   `json:"kind"`
	Nonce       string   `json:"nonce"`
	Account     string   `json:"account"`
	Calendar    string   `json:"calendar"`
	Title       string   `json:"title"`
	Start       string   `json:"start"`
	End         string   `json:"end"`
	TimeZone    string   `json:"time_zone"`
	Attendees   []string `json:"attendees"`
	Description string   `json:"description"`
	Location    string   `json:"location"`
	Meet        bool     `json:"meet"`
	ExpiresAt   int64    `json:"expires_at"`
}

func (o *Organiser) signEvent(p *EventPreview) string {
	raw, _ := json.Marshal(eventCanonical{
		Kind: "event", Nonce: hex.EncodeToString(p.Nonce[:]), Account: p.Account, Calendar: p.Calendar, Title: p.Title,
		Start: p.Start.Format(time.RFC3339), End: p.End.Format(time.RFC3339), TimeZone: p.TimeZone,
		Attendees: p.Attendees, Description: p.Description, Location: p.Location, Meet: p.Meet, ExpiresAt: p.Expires.Unix(),
	})
	m := hmac.New(sha256.New, o.key)
	m.Write(raw)
	return strconv.FormatInt(p.Expires.Unix(), 10) + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// EventID is the Google event id create_event uses for p. It is derived from
// the event's content (not from the preview's nonce): re-previewing the same
// event after an uncertain failure yields the same id, so Google answers 409
// instead of inviting everyone a second time. base32hex lowercase is within
// Google's id alphabet (a-v, 0-9); "mm" + 40 characters.
func (p *EventPreview) EventID() string {
	atts := append([]string(nil), p.Attendees...)
	sort.Strings(atts)
	raw, _ := json.Marshal([]any{
		p.Account, p.Calendar, p.Title, p.Start.Format(time.RFC3339), p.End.Format(time.RFC3339), p.TimeZone,
		atts, p.Description, p.Location, p.Meet,
	})
	sum := sha256.Sum256(raw)
	id := base32.HexEncoding.WithPadding(base32.NoPadding).EncodeToString(sum[:])
	return "mm" + strings.ToLower(id)[:40]
}

// DescriptionSHA256 is the hex SHA-256 of the exact description (of the empty
// string when there is none): what expect_description_sha256 restates.
func (p *EventPreview) DescriptionSHA256() string {
	sum := sha256.Sum256([]byte(p.Description))
	return hex.EncodeToString(sum[:])
}

// IssueEvent stamps p with a nonce, an expiry and a token, and remembers it.
func (o *Organiser) IssueEvent(p *EventPreview) (*EventPreview, error) {
	p.Expires = time.Now().Add(PreviewTTL).UTC().Truncate(time.Second)
	if _, err := rand.Read(p.Nonce[:]); err != nil {
		return nil, fmt.Errorf("organise: nonce: %w", err)
	}
	p.Token = o.signEvent(p)

	o.mu.Lock()
	defer o.mu.Unlock()
	if o.events == nil {
		o.events = map[string]*EventPreview{}
	}
	now := time.Now()
	kept := o.eventOrder[:0]
	for _, t := range o.eventOrder {
		if q, ok := o.events[t]; ok && now.Before(q.Expires) && t != p.Token {
			kept = append(kept, t)
		} else {
			delete(o.events, t)
		}
	}
	o.eventOrder = append(kept, p.Token)
	o.events[p.Token] = p
	for len(o.eventOrder) > maxPreviews {
		delete(o.events, o.eventOrder[0])
		o.eventOrder = o.eventOrder[1:]
	}
	return p, nil
}

// LookupEvent returns the event preview a token names, or ErrExpired.
func (o *Organiser) LookupEvent(token string) (*EventPreview, error) {
	o.mu.Lock()
	p, ok := o.events[token]
	o.mu.Unlock()
	if !ok || !time.Now().Before(p.Expires) {
		return nil, ErrExpired
	}
	if !hmac.Equal([]byte(o.signEvent(p)), []byte(token)) {
		return nil, ErrExpired
	}
	return p, nil
}

// ConsumeEvent forgets an event preview: a token approves one create_event.
// Of two racing calls exactly one gets true.
func (o *Organiser) ConsumeEvent(token string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	p, ok := o.events[token]
	if !ok {
		return false
	}
	live := time.Now().Before(p.Expires)
	delete(o.events, token)
	for i, t := range o.eventOrder {
		if t == token {
			o.eventOrder = append(o.eventOrder[:i], o.eventOrder[i+1:]...)
			break
		}
	}
	return live
}

// QuestionKeyEvent names the input request that asks the owner about p.
func (o *Organiser) QuestionKeyEvent(p *EventPreview) string {
	sum := sha256.Sum256([]byte(p.Token + hex.EncodeToString(p.Nonce[:])))
	return "approve-" + hex.EncodeToString(sum[:])[:16]
}

const (
	// UnelicitedInviteesPerHour is how many invitee addresses, summed over
	// events, one account may have invited in a rolling hour on the client's
	// tool approval alone (no elicitation).
	UnelicitedInviteesPerHour = 20
	// UnelicitedAttendeesPerEvent is the most attendees one event may have on
	// that path.
	UnelicitedAttendeesPerEvent = 10
)

type inviteTake struct {
	at time.Time
	n  int
}

// TakeUnelicitedInvitees counts n invitees created without elicitation
// against the account's rolling-hour cap and reports whether they fit. A
// refusal counts nothing.
func (o *Organiser) TakeUnelicitedInvitees(account string, n int) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.eventTimes == nil {
		o.eventTimes = map[string][]inviteTake{}
	}
	now := time.Now()
	kept := o.eventTimes[account][:0]
	total := 0
	for _, t := range o.eventTimes[account] {
		if now.Sub(t.at) < unelicitedWindow {
			kept = append(kept, t)
			total += t.n
		}
	}
	if total+n > UnelicitedInviteesPerHour {
		o.eventTimes[account] = kept
		return false
	}
	o.eventTimes[account] = append(kept, inviteTake{now, n})
	return true
}
