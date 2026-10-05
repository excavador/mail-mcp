package cache

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Bounds on a senders listing.
const (
	defaultSendersLimit = 20
	maxSendersLimit     = 100
	maxSenderQueryBytes = 256
)

// SenderSorts are the orders a listing accepts; all are descending (largest,
// latest or most recently first seen first), ties by address.
var SenderSorts = []string{"count", "last_at", "first_at"}

// ErrSenderNotFound is a sender with no row.
var ErrSenderNotFound = errors.New("sender not found")

// SenderQuery selects senders. Zero fields do not constrain.
type SenderQuery struct {
	Account      string // required
	Query        string // substring of address, domain or display name
	Kind         string
	MinMsgs      int
	NeverReplied bool      // n_replied_by_me = 0
	Since        time.Time // last_at at or after
	Sort         string
	Limit        int
	Offset       int
}

// SenderRow is one senders row, as the senders tool shows it.
type SenderRow struct {
	Addr         string
	Domain       string
	Name         string
	Kind         string
	KindSource   string
	NMsgs        int
	NRepliedByMe int
	NToMe        int
	FirstAt      time.Time
	LastAt       time.Time
	ListID       string
	HasUnsub     bool
}

// ListSenders returns a page of senders and the total that match.
func (c *Cache) ListSenders(ctx context.Context, q SenderQuery) ([]SenderRow, int, error) {
	if q.Account == "" {
		return nil, 0, errors.New("cache: senders: account is required")
	}
	if len(q.Query) > maxSenderQueryBytes {
		return nil, 0, fmt.Errorf("%w: query is longer than %d bytes", ErrQueryLimit, maxSenderQueryBytes)
	}
	if q.Kind != "" && !ValidSenderKind(q.Kind) {
		return nil, 0, fmt.Errorf("cache: senders: unknown kind %q", q.Kind)
	}
	limit := q.Limit
	switch {
	case limit <= 0:
		limit = defaultSendersLimit
	case limit > maxSendersLimit:
		limit = maxSendersLimit
	}
	order := `n_msgs DESC, addr`
	switch q.Sort {
	case "", "count":
	case "last_at":
		order = `last_at DESC, addr`
	case "first_at":
		order = `first_at DESC, addr`
	default:
		return nil, 0, fmt.Errorf("cache: senders: unknown sort %q", q.Sort)
	}
	where := `account = ? AND n_msgs > 0`
	args := []any{q.Account}
	if t := strings.TrimSpace(q.Query); t != "" {
		pat := "%" + likeEscaper.Replace(strings.ToLower(t)) + "%"
		where += ` AND (addr LIKE ? ESCAPE '\' OR domain LIKE ? ESCAPE '\' OR lower(display_names_json) LIKE ? ESCAPE '\')`
		args = append(args, pat, pat, pat)
	}
	if q.Kind != "" {
		where += ` AND kind = ?`
		args = append(args, q.Kind)
	}
	if q.MinMsgs > 0 {
		where += ` AND n_msgs >= ?`
		args = append(args, q.MinMsgs)
	}
	if q.NeverReplied {
		where += ` AND n_replied_by_me = 0`
	}
	if !q.Since.IsZero() {
		where += ` AND last_at >= ?`
		args = append(args, q.Since.Unix())
	}
	ctx, cancel := context.WithTimeout(ctx, searchTimeout)
	defer cancel()
	var total int
	if err := c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM senders WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("cache: senders: %w", err)
	}
	rows, err := c.db.QueryContext(ctx, `
SELECT addr, domain, display_names_json, kind, kind_source, n_msgs, n_replied_by_me, n_to_me, first_at, last_at, list_id, has_list_unsubscribe
FROM senders WHERE `+where+` ORDER BY `+order+` LIMIT ? OFFSET ?`, append(args, limit, max(q.Offset, 0))...)
	if err != nil {
		return nil, 0, fmt.Errorf("cache: senders: %w", err)
	}
	defer rows.Close()
	var out []SenderRow
	for rows.Next() {
		var (
			r           SenderRow
			names       string
			first, last int64
			unsub       int
		)
		if err := rows.Scan(&r.Addr, &r.Domain, &names, &r.Kind, &r.KindSource, &r.NMsgs, &r.NRepliedByMe, &r.NToMe, &first, &last, &r.ListID, &unsub); err != nil {
			return nil, 0, fmt.Errorf("cache: senders: %w", err)
		}
		var ns []string
		if json.Unmarshal([]byte(names), &ns) == nil && len(ns) > 0 {
			r.Name = ns[0]
		}
		if first > 0 {
			r.FirstAt = time.Unix(first, 0).UTC()
		}
		if last > 0 {
			r.LastAt = time.Unix(last, 0).UTC()
		}
		r.HasUnsub = unsub == 1
		out = append(out, r)
	}
	return out, total, rows.Err()
}

// SenderKindState is a sender's kind and where it came from.
type SenderKindState struct {
	Kind, Source string
}

// SetSenderKind makes kind the owner's decision for a sender and returns what
// it was before. The sender must have a row.
func (c *Cache) SetSenderKind(ctx context.Context, account, addr, kind string) (SenderKindState, error) {
	var old SenderKindState
	if !ValidSenderKind(kind) {
		return old, fmt.Errorf("cache: unknown sender kind %q", kind)
	}
	addr = BareAddr(addr)
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return old, fmt.Errorf("cache: set sender kind: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	err = tx.QueryRowContext(ctx, `SELECT kind, kind_source FROM senders WHERE account = ? AND addr = ?`, account, addr).Scan(&old.Kind, &old.Source)
	if errors.Is(err, sql.ErrNoRows) {
		return old, ErrSenderNotFound
	}
	if err != nil {
		return old, fmt.Errorf("cache: set sender kind: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE senders SET kind = ?, kind_source = 'owner', kind_updated_at = ? WHERE account = ? AND addr = ?`,
		kind, c.now().Unix(), account, addr); err != nil {
		return old, fmt.Errorf("cache: set sender kind: %w", err)
	}
	return old, tx.Commit()
}

// ErrSenderChanged is an undo refused because the sender's kind is no longer
// what the undone change left it as.
var ErrSenderChanged = errors.New("sender kind changed since")

// RestoreSenderKind undoes SetSenderKind: it puts back the previous kind and
// source, provided the sender is still as that change left it (kind
// currentKind, set by the owner). A previous source of "rule" re-runs the
// rules on the sender's counts instead of restoring a possibly stale kind.
func (c *Cache) RestoreSenderKind(ctx context.Context, account, addr, currentKind string, prev SenderKindState) error {
	addr = BareAddr(addr)
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("cache: restore sender kind: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var (
		kind, source, domain                        string
		n, replied, nList, nUnsub, nGH, nTxn, nAuto int
	)
	err = tx.QueryRowContext(ctx, `SELECT kind, kind_source, domain, n_msgs, n_replied_by_me, n_list, n_unsub, n_gh, n_txn_subj, n_auto
FROM senders WHERE account = ? AND addr = ?`, account, addr).Scan(&kind, &source, &domain, &n, &replied, &nList, &nUnsub, &nGH, &nTxn, &nAuto)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrSenderNotFound
	}
	if err != nil {
		return fmt.Errorf("cache: restore sender kind: %w", err)
	}
	if kind != currentKind || source != SourceOwner {
		return ErrSenderChanged
	}
	nk, ns := prev.Kind, prev.Source
	if ns == SourceRule {
		nk = ClassifySender(KindInputs{Addr: addr, Domain: domain, NMsgs: n, NReplied: replied, NList: nList, NUnsub: nUnsub, NGH: nGH, NTxn: nTxn, NAuto: nAuto})
	}
	if _, err := tx.ExecContext(ctx, `UPDATE senders SET kind = ?, kind_source = ?, kind_updated_at = ? WHERE account = ? AND addr = ?`,
		nk, ns, c.now().Unix(), account, addr); err != nil {
		return fmt.Errorf("cache: restore sender kind: %w", err)
	}
	return tx.Commit()
}
