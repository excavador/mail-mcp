package organise

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"time"
)

// DraftPreview is a rendered draft awaiting approval: the exact bytes that
// create_draft will append, and what the owner is shown about them. It lives in
// memory only, for PreviewTTL.
type DraftPreview struct {
	Token   string
	Expires time.Time
	Nonce   [16]byte

	Account string
	Folder  string // the Drafts folder, as found when previewed
	// Raw is the whole message; the token covers its digest, so what is
	// appended is what was previewed.
	Raw []byte

	// What the echo fields restate and the history records (never the body).
	From, Subject, MessageID, ReplyToID string
	To, Cc, Bcc                         []string
}

type draftCanonical struct {
	Kind      string `json:"kind"`
	Nonce     string `json:"nonce"`
	Account   string `json:"account"`
	Folder    string `json:"folder"`
	RawSHA256 string `json:"raw_sha256"`
	ExpiresAt int64  `json:"expires_at"`
}

func (o *Organiser) signDraft(p *DraftPreview) string {
	sum := sha256.Sum256(p.Raw)
	raw, _ := json.Marshal(draftCanonical{
		Kind: "draft", Nonce: hex.EncodeToString(p.Nonce[:]), Account: p.Account, Folder: p.Folder,
		RawSHA256: hex.EncodeToString(sum[:]), ExpiresAt: p.Expires.Unix(),
	})
	m := hmac.New(sha256.New, o.key)
	m.Write(raw)
	return strconv.FormatInt(p.Expires.Unix(), 10) + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// IssueDraft stamps p with a nonce, an expiry and a token, and remembers it.
func (o *Organiser) IssueDraft(p *DraftPreview) (*DraftPreview, error) {
	p.Expires = time.Now().Add(PreviewTTL).UTC().Truncate(time.Second)
	if _, err := rand.Read(p.Nonce[:]); err != nil {
		return nil, fmt.Errorf("organise: nonce: %w", err)
	}
	p.Token = o.signDraft(p)

	o.mu.Lock()
	defer o.mu.Unlock()
	now := time.Now()
	kept := o.draftOrder[:0]
	for _, t := range o.draftOrder {
		if q, ok := o.drafts[t]; ok && now.Before(q.Expires) && t != p.Token {
			kept = append(kept, t)
		} else {
			delete(o.drafts, t)
		}
	}
	o.draftOrder = append(kept, p.Token)
	o.drafts[p.Token] = p
	for len(o.draftOrder) > maxPreviews {
		delete(o.drafts, o.draftOrder[0])
		o.draftOrder = o.draftOrder[1:]
	}
	return p, nil
}

// LookupDraft returns the draft a token names, or ErrExpired: unknown, evicted,
// expired and tampered tokens are not told apart.
func (o *Organiser) LookupDraft(token string) (*DraftPreview, error) {
	o.mu.Lock()
	p, ok := o.drafts[token]
	o.mu.Unlock()
	if !ok || !time.Now().Before(p.Expires) {
		return nil, ErrExpired
	}
	if !hmac.Equal([]byte(o.signDraft(p)), []byte(token)) {
		return nil, ErrExpired
	}
	return p, nil
}

// ConsumeDraft forgets a draft preview: a token approves one create_draft. Of
// two racing calls exactly one gets true.
func (o *Organiser) ConsumeDraft(token string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	p, ok := o.drafts[token]
	if !ok {
		return false
	}
	live := time.Now().Before(p.Expires)
	delete(o.drafts, token)
	for i, t := range o.draftOrder {
		if t == token {
			o.draftOrder = append(o.draftOrder[:i], o.draftOrder[i+1:]...)
			break
		}
	}
	return live
}
