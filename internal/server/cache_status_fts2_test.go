package server

import (
	"path/filepath"
	"testing"

	"github.com/excavador/mail-mcp/internal/cache"
)

func TestCacheStatusReportsFts2Backfill(t *testing.T) {
	c, err := cache.Open(filepath.Join(t.TempDir(), "cache"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	for _, mode := range []Mode{Read, Admin} {
		t.Run(mode.String(), func(t *testing.T) {
			cs := connectServer(t, New(nil, c, "test", mode))
			type out struct {
				Backfill *struct {
					Done     *int  `json:"done"`
					Total    *int  `json:"total"`
					Complete *bool `json:"complete"`
				} `json:"fts2_backfill"`
			}
			o, raw := ok[out](t, cs, "cache_status", map[string]any{})
			b := o.Backfill
			if b == nil || b.Done == nil || b.Total == nil || b.Complete == nil {
				t.Fatalf("fts2_backfill {done,total,complete} missing: %s", raw)
			}
			if !*b.Complete || *b.Done != *b.Total {
				t.Fatalf("complete backfill must have done == total: %s", raw)
			}
		})
	}
}
