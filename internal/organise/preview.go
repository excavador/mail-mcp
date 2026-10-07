package organise

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/excavador/mail-mcp/internal/accounts"
	"github.com/excavador/mail-mcp/internal/cache"
)

const (
	// PreviewTTL is how long an approval-pending preview stays valid.
	PreviewTTL = 15 * time.Minute
	// maxPreviews bounds the in-memory previews; the oldest is evicted.
	maxPreviews = 32
	// SampleCount is how many matching messages a preview shows.
	SampleCount = 20
)

// Preview is a resolved intent awaiting approval: a fixed set of stable ids.
type Preview struct {
	Token   string
	Expires time.Time

	Account string
	Intent  Intent
	// Nonce makes every token unique, even for an identical preview.
	Nonce [16]byte
	// question is the nonce of the one outstanding approval question (zero:
	// none). It is single-use and guarded by Organiser.mu.
	question [16]byte
	Kind     string // KindApply, KindUndo or KindReapply
	// IDs is the sorted, de-duplicated set the owner is shown and approves.
	IDs []string
	// IDsOnly means the set is explicit (undo) and the criterion carries only
	// its folder; otherwise the criterion is re-resolved at apply.
	IDsOnly bool
	// After restricts a re-resolve to mail the server received after it
	// (reapply).
	After time.Time

	// CopyBack (undo of a move) lists ids that were already in the move's
	// target before it. Undo cannot MOVE them back without stripping the
	// target they had, so apply COPYs them into Intent.Target and leaves them
	// in the folder they are in. Sorted; counted in Matched.
	CopyBack []string

	Undoes, Reapplies string // history id this preview reverses or repeats
	Missing           int    // undo: recorded messages not found in the folder

	Matched int
	Samples []cache.SearchHit
}

// Organiser holds the previews and the per-account write slots. One Organiser
// is shared by every server in the process.
type Organiser struct {
	store *cache.Cache
	key   []byte // HMAC key; in memory, so a restart invalidates every token

	mu       sync.Mutex
	previews map[string]*Preview
	order    []string // tokens, oldest first

	slots sync.Map // account -> chan struct{} (capacity 1)

	// drafts are the previews of preview_draft, kept apart from the intents: a
	// draft token never opens apply_intent and an intent token never opens
	// create_draft. They share the signing key, the lifetime and the cap.
	drafts     map[string]*DraftPreview
	draftOrder []string
}

// New returns an Organiser over store with a fresh random signing key.
func New(store *cache.Cache) (*Organiser, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("organise: signing key: %w", err)
	}
	return &Organiser{store: store, key: key, previews: map[string]*Preview{}, drafts: map[string]*DraftPreview{}}, nil
}

// Acquire takes the account's write slot. A second caller does not wait: it is
// refused, so two applies can never interleave on one mailbox.
func (o *Organiser) Acquire(account string) (release func(), err error) {
	v, _ := o.slots.LoadOrStore(account, make(chan struct{}, 1))
	slot := v.(chan struct{})
	select {
	case slot <- struct{}{}:
		return func() { <-slot }, nil
	default:
		return nil, ErrBusy
	}
}

func (o *Organiser) query(in Intent, after time.Time) cache.MemberQuery {
	c := in.Criterion
	return cache.MemberQuery{
		Folder: c.Folder, From: c.From, To: c.To, SubjectContains: c.SubjectContains,
		ListID: c.ListID, GitHubReason: c.GitHubReason, Tag: c.Tag, Since: c.Since, Before: c.Before,
		ReceivedAfter: after,
	}
}

// resolve returns the cached members the preview's criterion (or explicit id
// set) covers right now.
func (o *Organiser) resolve(ctx context.Context, p *Preview) ([]cache.Member, error) {
	if p.IDsOnly {
		return o.store.MembersByID(ctx, p.Account, p.Intent.Criterion.Folder, p.IDs)
	}
	ms, err := o.store.ResolveMembers(ctx, p.Account, o.query(p.Intent, p.After), maxIntentMessages)
	if errors.Is(err, cache.ErrTooMany) {
		return nil, ErrTooManyMatched
	}
	return ms, err
}

func uniqueIDs(ms []cache.Member) []string {
	seen := make(map[string]bool, len(ms))
	var ids []string
	for _, m := range ms {
		if !seen[m.StableID] {
			seen[m.StableID] = true
			ids = append(ids, m.StableID)
		}
	}
	return ids // already in resolve order (newest first); callers sort a copy
}

// PreviewIntent resolves a criterion intent against the cache and issues a
// token for it. kind is KindApply or KindReapply; after, for a reapply, is
// the original apply time.
func (o *Organiser) PreviewIntent(ctx context.Context, a accounts.Account, in Intent, kind string, after time.Time, reapplies string) (*Preview, error) {
	in.Account = a.Name
	if err := in.Validate(a.Provider); err != nil {
		return nil, err
	}
	if err := o.checkTarget(ctx, a.Name, in.Target); err != nil {
		return nil, err
	}
	p := &Preview{Account: a.Name, Intent: in, Kind: kind, After: after, Reapplies: reapplies}
	ms, err := o.resolve(ctx, p)
	if err != nil {
		return nil, err
	}
	return o.issue(ctx, p, uniqueIDs(ms))
}

// PreviewIDs issues a token for an explicit set of ids in in.Criterion.Folder
// (undo). The caller has already found them; missing counts the ones it could
// not.
func (o *Organiser) PreviewIDs(ctx context.Context, a accounts.Account, in Intent, ids, copyBack []string, missing int, undoes string) (*Preview, error) {
	in.Account = a.Name
	if err := in.CheckMove(a.Provider); err != nil {
		return nil, err
	}
	if err := o.checkTarget(ctx, a.Name, in.Target); err != nil {
		return nil, err
	}
	if len(ids)+len(copyBack) > maxIntentMessages {
		return nil, ErrTooManyMatched
	}
	p := &Preview{Account: a.Name, Intent: in, Kind: KindUndo, IDsOnly: true, Undoes: undoes, Missing: missing}
	p.CopyBack = append([]string(nil), copyBack...)
	sort.Strings(p.CopyBack)
	return o.issue(ctx, p, ids)
}

// checkTarget refuses a preview whose target folder the cache does not know
// for the account. It answers from the cache's folders table (create_folder
// records a new folder there at once), so there is no server round trip;
// apply still re-checks with LIST.
func (o *Organiser) checkTarget(ctx context.Context, account, target string) error {
	ok, err := o.store.HasFolder(ctx, account, target)
	if err != nil {
		return err
	}
	if !ok {
		return refusef(errTargetMissingFmt, target, account)
	}
	return nil
}

func (o *Organiser) issue(ctx context.Context, p *Preview, ids []string) (*Preview, error) {
	p.Matched = len(ids) + len(p.CopyBack)
	sample := append(append([]string(nil), ids...), p.CopyBack...)
	sample = sample[:min(len(sample), SampleCount)] // resolve order is newest first
	hits, err := o.store.Summaries(ctx, p.Account, sample)
	if err != nil {
		return nil, err
	}
	p.Samples = hits
	p.IDs = append([]string(nil), ids...)
	sort.Strings(p.IDs)
	p.Expires = time.Now().Add(PreviewTTL).UTC().Truncate(time.Second)
	if _, err := rand.Read(p.Nonce[:]); err != nil {
		return nil, fmt.Errorf("organise: nonce: %w", err)
	}
	p.Token = o.sign(p)

	o.mu.Lock()
	defer o.mu.Unlock()
	now := time.Now()
	kept := o.order[:0]
	for _, t := range o.order {
		if q, ok := o.previews[t]; ok && now.Before(q.Expires) && t != p.Token {
			kept = append(kept, t)
		} else {
			delete(o.previews, t) // expired (or a duplicate of the new token)
		}
	}
	o.order = kept
	o.previews[p.Token] = p
	o.order = append(o.order, p.Token)
	for len(o.order) > maxPreviews {
		delete(o.previews, o.order[0])
		o.order = o.order[1:]
	}
	return p, nil
}

// canonical is what the token's HMAC covers. Struct field order is fixed, so
// the JSON is deterministic.
type canonical struct {
	Nonce     string    `json:"nonce"`
	Account   string    `json:"account"`
	Kind      string    `json:"kind"`
	Intent    Intent    `json:"intent"`
	IDsOnly   bool      `json:"ids_only"`
	After     time.Time `json:"after"`
	IDDigest  string    `json:"ids_sha256"`
	CopyBack  string    `json:"copy_back_sha256"`
	ExpiresAt int64     `json:"expires_at"`
}

func (o *Organiser) sign(p *Preview) string {
	sum := sha256.Sum256([]byte(strings.Join(p.IDs, "\n")))
	cb := sha256.Sum256([]byte(strings.Join(p.CopyBack, "\n")))
	raw, _ := json.Marshal(canonical{
		CopyBack: hex.EncodeToString(cb[:]),
		Nonce:    hex.EncodeToString(p.Nonce[:]), Account: p.Account, Kind: p.Kind, Intent: p.Intent, IDsOnly: p.IDsOnly, After: p.After.UTC(),
		IDDigest: hex.EncodeToString(sum[:]), ExpiresAt: p.Expires.Unix(),
	})
	m := hmac.New(sha256.New, o.key)
	m.Write(raw)
	// The expiry rides in the token, ahead of the MAC it is covered by.
	return strconv.FormatInt(p.Expires.Unix(), 10) + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// Lookup returns the preview a token names, or ErrExpired: unknown, evicted,
// expired and tampered tokens are not told apart.
func (o *Organiser) Lookup(token string) (*Preview, error) {
	o.mu.Lock()
	p, ok := o.previews[token]
	o.mu.Unlock()
	if !ok || !time.Now().Before(p.Expires) {
		return nil, ErrExpired
	}
	if !hmac.Equal([]byte(o.sign(p)), []byte(token)) {
		return nil, ErrExpired
	}
	return p, nil
}

// Consume forgets a preview: a token approves one apply. It reports whether
// the preview was still there and unexpired, so of two racing applies exactly
// one gets true.
func (o *Organiser) Consume(token string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	p, ok := o.previews[token]
	if !ok {
		return false
	}
	live := time.Now().Before(p.Expires)
	delete(o.previews, token)
	for i, t := range o.order {
		if t == token {
			o.order = append(o.order[:i], o.order[i+1:]...)
			break
		}
	}
	return live
}

// QuestionKey names the input request that asks the owner about p: derived
// from the token and the preview's nonce, so it differs for every preview.
func (o *Organiser) QuestionKey(p *Preview) string {
	sum := sha256.Sum256([]byte(p.Token + hex.EncodeToString(p.Nonce[:])))
	return "approve-" + hex.EncodeToString(sum[:])[:16]
}

func (o *Organiser) questionMAC(token string, nonce []byte) []byte {
	m := hmac.New(sha256.New, o.key)
	m.Write([]byte("question|" + token + "|"))
	m.Write(nonce)
	return m.Sum(nil)
}

// NewQuestion arms a fresh question for the token's preview and returns the
// RequestState that must come back with its answer. A question asked earlier
// for the same preview stops being valid.
func (o *Organiser) NewQuestion(token string) (string, error) {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	p, ok := o.previews[token]
	if !ok {
		return "", ErrExpired
	}
	p.question = nonce
	return base64.RawURLEncoding.EncodeToString(nonce[:]) + "." +
		base64.RawURLEncoding.EncodeToString(o.questionMAC(token, nonce[:])), nil
}

// TakeQuestion checks a RequestState against the token's outstanding question
// and, if it is that question, uses it up: a state is good for one answer.
func (o *Organiser) TakeQuestion(token, state string) bool {
	n, mac, ok := strings.Cut(state, ".")
	if !ok {
		return false
	}
	nonce, err1 := base64.RawURLEncoding.DecodeString(n)
	got, err2 := base64.RawURLEncoding.DecodeString(mac)
	if err1 != nil || err2 != nil || len(nonce) != 16 || !hmac.Equal(got, o.questionMAC(token, nonce)) {
		return false
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	p, ok := o.previews[token]
	var zero [16]byte
	if !ok || p.question == zero || !hmac.Equal(p.question[:], nonce) {
		return false
	}
	p.question = zero
	return true
}
