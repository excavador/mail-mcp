package server

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestParseApprovalMode(t *testing.T) {
	for in, want := range map[string]ApprovalMode{"client": ApprovalClient, "elicitation": ApprovalElicitation} {
		got, err := ParseApprovalMode(in)
		if err != nil || got != want {
			t.Errorf("ParseApprovalMode(%q) = %q, %v", in, got, err)
		}
	}
	// Matching is exact: empty and different case are errors, not defaults.
	for _, in := range []string{"", "Client", "ELICITATION", "bogus", " client"} {
		if got, err := ParseApprovalMode(in); err == nil || got != "" {
			t.Errorf("ParseApprovalMode(%q) = %q, %v; want an error", in, got, err)
		}
	}
}

// countingElicit records every elicitation request and accepts it.
func countingElicit(n *atomic.Int32) func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
	return func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
		n.Add(1)
		return &mcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}, nil
	}
}

func TestClientModeNeverElicitsEvenWhenTheClientAdvertisesIt(t *testing.T) {
	for _, proto := range []struct {
		name string
		old  bool
	}{{"2025-11-25", true}, {"2026-07-28", false}} {
		t.Run(proto.name, func(t *testing.T) {
			e := newWEnv(t, true, gmailPair, "INBOX", "Work")
			e.addMany("INBOX", "ok@example.com", 50)
			e.addMany("INBOX", "bulk@example.com", 51)
			e.refresh("acct")
			e.log.reset()
			var asked atomic.Int32
			// No WithApprovalMode: client is the default.
			cs := e.connectP(Admin, countingElicit(&asked), proto.old, WithHistory(e.hist), WithOrganiser(e.org))

			p := preview(t, cs, "acct", "INBOX", fromCrit("ok@example.com"), "Work", "move")
			if a := apply(t, cs, p); a.Done != 50 || a.ApprovedBy != "client-tool-approval" {
				t.Fatalf("apply of 50 = %+v", a)
			}
			if h, _ := listHistory(t, cs, ""); h.Records[0].Preview.ApprovedBy != "client-tool-approval" {
				t.Errorf("recorded approved_by = %q", h.Records[0].Preview.ApprovedBy)
			}

			big := preview(t, cs, "acct", "INBOX", fromCrit("bulk@example.com"), "Work", "move")
			e.log.reset()
			requireToolError(t, cs, "apply_intent", applyArgs(big), "more than 50 messages cannot be applied on the client's tool approval alone")
			e.noWrites(t)
			if n := e.serverCount("INBOX"); n != 51 {
				t.Errorf("INBOX = %d, want the 51 untouched", n)
			}

			// An echo mismatch is refused in client mode as before.
			small := preview(t, cs, "acct", "INBOX", fromCrit("bulk@example.com"), "Work", "label")
			args := applyArgs(small)
			args["expect_matched"] = small.Matched + 1
			requireToolError(t, cs, "apply_intent", args, "expect_matched does not match the preview")

			if n := asked.Load(); n != 0 {
				t.Errorf("elicitation handler called %d times in client mode", n)
			}
		})
	}
}

func TestApplyDescriptionFollowsTheApprovalMode(t *testing.T) {
	desc := func(opts ...Option) string {
		e := basic(t, true)
		cs := e.connect(Admin, nil, append([]Option{WithHistory(e.hist), WithOrganiser(e.org)}, opts...)...)
		return toolNames(t, cs)["apply_intent"].Description
	}
	for name, d := range map[string]string{"default": desc(), "explicit client": desc(WithApprovalMode(ApprovalClient))} {
		for _, want := range []string{"tool-approval prompt", "above 50 messages the apply is refused"} {
			if !strings.Contains(d, want) {
				t.Errorf("%s description lacks %q: %s", name, want, d)
			}
		}
	}
	d := desc(WithApprovalMode(ApprovalElicitation))
	for _, unwanted := range []string{"tool-approval", "above 50 messages", "refused"} {
		if strings.Contains(d, unwanted) {
			t.Errorf("elicitation description mentions %q: %s", unwanted, d)
		}
	}
	if !strings.Contains(d, "Requires approved=true") {
		t.Errorf("elicitation description lost its base text: %s", d)
	}
	if got := desc(WithApprovalMode(ApprovalElicitation), WithMaxUnelicited(7)); strings.Contains(got, "7 messages") {
		t.Errorf("elicitation description mentions the cap: %s", got)
	}
	if got := desc(WithMaxUnelicited(7)); !strings.Contains(got, "above 7 messages") {
		t.Errorf("client description does not follow the cap: %s", got)
	}
}

func TestElicitationModeWithoutClientCapabilityFallsBackToClientToolApproval(t *testing.T) {
	e := newWEnv(t, true, gmailPair, "INBOX", "Work")
	e.addMany("INBOX", "ok@example.com", 3)
	e.addMany("INBOX", "bulk@example.com", 51)
	e.refresh("acct")
	cs := e.connect(Admin, nil, WithHistory(e.hist), WithOrganiser(e.org), WithApprovalMode(ApprovalElicitation))
	if a := apply(t, cs, preview(t, cs, "acct", "INBOX", fromCrit("ok@example.com"), "Work", "move")); a.Done != 3 || a.ApprovedBy != "client-tool-approval" {
		t.Fatalf("apply = %+v", a)
	}
	big := preview(t, cs, "acct", "INBOX", fromCrit("bulk@example.com"), "Work", "move")
	requireToolError(t, cs, "apply_intent", applyArgs(big), "more than 50 messages cannot be applied on the client's tool approval alone")
}
