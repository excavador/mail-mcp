package history

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestCreateEventRecordRoundTripsAndHasNoDescription(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	want := &EventInfo{
		Calendar: "primary", EventID: "mmabcdefghijklmnopqrstuvabcdefghijklmnopq", HTMLLink: "https://calendar.example/e/1",
		Title: "Planning", Start: "2030-10-12T14:00:00+02:00", End: "2030-10-12T15:00:00+02:00", TimeZone: "Europe/Amsterdam",
		Attendees: []string{"a@example.com", "b@example.org"}, Meet: true,
	}
	saved, err := s.Append(Record{Account: "acct", Kind: KindCreateEvent, Target: "primary", Event: want,
		Preview: PreviewInfo{ApprovedBy: ApprovedElicitation}})
	if err != nil {
		t.Fatal(err)
	}
	if KindCreateEvent != "create_event" {
		t.Errorf("KindCreateEvent = %q", KindCreateEvent)
	}
	// An unknown outcome carries its error and the planned id.
	if _, err := s.Append(Record{Account: "acct", Kind: KindCreateEvent, Target: "primary", Event: &EventInfo{EventID: "mmx", Title: "T"},
		Error: "the event may or may not have been created"}); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, mustOne(t, dir)))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "description") {
		t.Errorf("the history has a description field: %s", raw)
	}
	_ = s.Close()

	s2 := open(t, dir)
	got, found := s2.Get(saved.ID)
	if !found || !reflect.DeepEqual(got.Event, want) || got.Kind != KindCreateEvent || got.Preview.ApprovedBy != ApprovedElicitation {
		t.Fatalf("reloaded = %+v (%v)", got, found)
	}
	recs := s2.List("acct", 10)
	if len(recs) != 2 || !strings.Contains(recs[0].Error, "may or may not") || recs[0].Event.EventID != "mmx" {
		t.Errorf("records = %+v", recs)
	}
}

func mustOne(t *testing.T, dir string) string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil || len(ents) == 0 {
		t.Fatalf("history dir: %v %v", ents, err)
	}
	for _, e := range ents {
		if !e.IsDir() {
			return e.Name()
		}
	}
	t.Fatal("no file")
	return ""
}
