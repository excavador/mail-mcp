package server

import (
	"testing"
)

func TestSendersTagsAndSavedQueriesEndToEnd(t *testing.T) {
	e := newWEnv(t, true, gmailPair, "INBOX")
	e.add("INBOX", "1", "Shop <no-reply@shop.example>", "Your order 9 has shipped")
	e.add("INBOX", "2", "Alice <alice@x.example>", "lunch")
	e.refresh("acct")
	e.log.reset()
	cs := e.admin()

	type sendersT struct {
		Total   int `json:"total"`
		Senders []struct {
			Addr       string `json:"addr"`
			Kind       string `json:"kind"`
			KindSource string `json:"kind_source"`
			NMsgs      int    `json:"n_msgs"`
		} `json:"senders"`
	}
	// The gate query.
	got, _ := ok[sendersT](t, cs, "senders", map[string]any{"account": "acct", "kind": "transactional", "never_replied": true})
	if got.Total != 1 || got.Senders[0].Addr != "no-reply@shop.example" || got.Senders[0].KindSource != "rule" {
		t.Fatalf("gate query: %+v", got)
	}

	// set_sender_kind and its undo.
	type setT struct {
		OldKind   string `json:"old_kind"`
		NewKind   string `json:"new_kind"`
		HistoryID string `json:"history_id"`
	}
	set, _ := ok[setT](t, cs, "set_sender_kind", map[string]any{"account": "acct", "addr": "No-Reply@shop.example", "kind": "human"})
	if set.OldKind != "transactional" || set.NewKind != "human" || set.HistoryID == "" {
		t.Fatalf("set_sender_kind: %+v", set)
	}
	if got, _ = ok[sendersT](t, cs, "senders", map[string]any{"account": "acct", "kind": "transactional"}); got.Total != 0 {
		t.Errorf("owner kind not applied: %+v", got)
	}
	if got, _ = ok[sendersT](t, cs, "senders", map[string]any{"account": "acct", "kind": "human", "query": "shop"}); got.Total != 1 || got.Senders[0].KindSource != "owner" {
		t.Errorf("owner row: %+v", got)
	}
	ok[map[string]any](t, cs, "undo", map[string]any{"history_id": set.HistoryID})
	if got, _ = ok[sendersT](t, cs, "senders", map[string]any{"account": "acct", "kind": "transactional"}); got.Total != 1 {
		t.Errorf("undo did not restore the rule kind: %+v", got)
	}
	if res := call(t, cs, "undo", map[string]any{"history_id": set.HistoryID}); !res.IsError {
		t.Error("second undo of the same record accepted")
	}
	if res := call(t, cs, "set_sender_kind", map[string]any{"account": "acct", "addr": "alice@x.example", "kind": "bogus"}); !res.IsError {
		t.Error("bad kind accepted")
	}

	// Tags: dry run, tag, list, search, untag, undo.
	type tagT struct {
		Matched   int    `json:"matched"`
		Changed   int    `json:"changed"`
		DryRun    bool   `json:"dry_run"`
		HistoryID string `json:"history_id"`
	}
	dry, _ := ok[tagT](t, cs, "tag_messages", map[string]any{"account": "acct", "tag": "case/shop", "query": "order", "dry_run": true})
	if dry.Matched != 1 || dry.Changed != 0 || dry.HistoryID != "" {
		t.Fatalf("dry run: %+v", dry)
	}
	tg, _ := ok[tagT](t, cs, "tag_messages", map[string]any{"account": "acct", "tag": "Case/Shop", "query": "order"})
	if tg.Matched != 1 || tg.Changed != 1 || tg.HistoryID == "" {
		t.Fatalf("tag: %+v", tg)
	}
	type tagsT struct {
		Tags []struct {
			Tag   string `json:"tag"`
			Count int    `json:"count"`
		} `json:"tags"`
	}
	if l, _ := ok[tagsT](t, cs, "list_tags", map[string]any{"account": "acct"}); len(l.Tags) != 1 || l.Tags[0].Tag != "case/shop" || l.Tags[0].Count != 1 {
		t.Errorf("list_tags: %+v", l)
	}
	type searchT struct {
		Total int `json:"total"`
	}
	if s, _ := ok[searchT](t, cs, "search", map[string]any{"account": "acct", "tag": "case/shop"}); s.Total != 1 {
		t.Errorf("search tag: %+v", s)
	}
	if s, _ := ok[searchT](t, cs, "search", map[string]any{"account": "acct", "tag": "case/none"}); s.Total != 0 {
		t.Errorf("search other tag: %+v", s)
	}
	// Saved query.
	ok[map[string]any](t, cs, "save_query", map[string]any{"account": "acct", "name": "shop.case", "tag": "case/shop", "note": "n"})
	if s, _ := ok[searchT](t, cs, "search", map[string]any{"account": "acct", "saved": "shop.case"}); s.Total != 1 {
		t.Errorf("saved search: %+v", s)
	}
	if res := call(t, cs, "search", map[string]any{"account": "acct", "saved": "nope"}); !res.IsError {
		t.Error("unknown saved query accepted")
	}
	// Selector rules and bad tags.
	for _, args := range []map[string]any{
		{"account": "acct", "tag": "x"},
		{"account": "acct", "tag": "x", "query": "order", "tid": "t"},
		{"account": "acct", "tag": "Bad Tag", "query": "order"},
	} {
		if res := call(t, cs, "tag_messages", args); !res.IsError {
			t.Errorf("accepted %v", args)
		}
	}
	un, _ := ok[tagT](t, cs, "untag_messages", map[string]any{"account": "acct", "tag": "case/shop", "query": "order"})
	if un.Changed != 1 || un.HistoryID == "" {
		t.Fatalf("untag: %+v", un)
	}
	ok[map[string]any](t, cs, "undo", map[string]any{"history_id": un.HistoryID}) // tag is back
	if s, _ := ok[searchT](t, cs, "search", map[string]any{"account": "acct", "tag": "case/shop"}); s.Total != 1 {
		t.Errorf("untag undo: %+v", s)
	}
	// Undo of the untag restored the tag under its ORIGINAL history id, so
	// undoing the original tag_messages still removes it.
	if r, _ := ok[map[string]any](t, cs, "undo", map[string]any{"history_id": tg.HistoryID}); r["untagged"] != float64(1) {
		t.Errorf("undo of the original tag removed %v, want 1", r["untagged"])
	}
	if s, _ := ok[searchT](t, cs, "search", map[string]any{"account": "acct", "tag": "case/shop"}); s.Total != 0 {
		t.Errorf("tag undo: %+v", s)
	}

	// Read server: the read tools, none of the writers.
	rd := e.connect(Read, nil, WithHistory(e.hist), WithOrganiser(e.org))
	have := toolNames(t, rd)
	for _, n := range []string{"senders", "list_tags", "list_saved_queries"} {
		if have[n] == nil || !have[n].Annotations.ReadOnlyHint {
			t.Errorf("read server: %s missing or not read-only", n)
		}
	}
	for _, n := range []string{"set_sender_kind", "tag_messages", "untag_messages", "save_query"} {
		if have[n] != nil {
			t.Errorf("read server carries %s", n)
		}
	}
	// No IMAP writes happened for any of it.
	e.noWrites(t)
}
