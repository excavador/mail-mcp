package leader

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	coordv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

const (
	// The Lease records its duration in whole seconds, so a sub-second
	// LeaseDuration is read back as 0 (always expired) by a standby. Keep it
	// at a whole number of seconds; the renew and retry periods can be short.
	tLease = 2 * time.Second
	tRenew = 1 * time.Second
	tRetry = 100 * time.Millisecond
)

// syncBuf is a goroutine-safe log sink.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func newLog() (*slog.Logger, *syncBuf) {
	b := &syncBuf{}
	return slog.New(slog.NewTextHandler(b, nil)), b
}

func cfg(c *fake.Clientset, id string) Config {
	return Config{
		Mode: "true", Name: "lease", Namespace: "ns", Identity: id, Client: c,
		LeaseDuration: tLease, RenewDeadline: tRenew, Retry: tRetry,
	}
}

func holder(t *testing.T, c *fake.Clientset) (string, bool) {
	t.Helper()
	l, err := c.CoordinationV1().Leases("ns").Get(context.Background(), "lease", metav1.GetOptions{})
	if err != nil {
		return "", false
	}
	if l.Spec.HolderIdentity == nil {
		return "", true
	}
	return *l.Spec.HolderIdentity, true
}

func eventually(t *testing.T, d time.Duration, what string, f func() bool) {
	t.Helper()
	dl := time.Now().Add(d)
	for time.Now().Before(dl) {
		if f() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// start runs Run in a goroutine and returns a stop func that cancels and waits.
func start(t *testing.T, ctx context.Context, log *slog.Logger, c Config, jobs func(context.Context)) (done chan error) {
	t.Helper()
	done = make(chan error, 1)
	go func() { done <- Run(ctx, log, c, jobs) }()
	return done
}

func waitRun(t *testing.T, done chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return")
	}
}

func TestModeFalseRunsJobsImmediatelyWithoutClient(t *testing.T) {
	log, _ := newLog()
	for _, m := range []string{"false", "off", "no", "0", "FALSE"} {
		called := false
		err := Run(context.Background(), log, Config{Mode: m}, func(context.Context) { called = true })
		if err != nil || !called {
			t.Errorf("mode %q: err=%v called=%v", m, err, called)
		}
	}
}

func TestModeTrueOutsideClusterErrors(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")
	log, _ := newLog()
	called := false
	err := Run(context.Background(), log, Config{Mode: "true"}, func(context.Context) { called = true })
	if err == nil || called {
		t.Fatalf("err=%v called=%v, want an error and no jobs", err, called)
	}
}

func TestModeAutoOutsideClusterBehavesAsFalse(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")
	log, _ := newLog()
	for _, m := range []string{"auto", ""} {
		called := false
		if err := Run(context.Background(), log, Config{Mode: m}, func(context.Context) { called = true }); err != nil || !called {
			t.Errorf("mode %q: err=%v called=%v", m, err, called)
		}
	}
}

func TestInvalidModeErrors(t *testing.T) {
	log, _ := newLog()
	called := false
	err := Run(context.Background(), log, Config{Mode: "sometimes", Client: fake.NewClientset()}, func(context.Context) { called = true })
	if err == nil || !strings.Contains(err.Error(), "sometimes") || called {
		t.Fatalf("err=%v called=%v", err, called)
	}
}

func TestOnlyOneOfTwoRunsJobs(t *testing.T) {
	c := fake.NewClientset()
	log, _ := newLog()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu sync.Mutex
	running := map[string]int{}
	jobs := func(id string) func(context.Context) {
		return func(jc context.Context) {
			mu.Lock()
			running[id]++
			mu.Unlock()
			<-jc.Done()
		}
	}
	da := start(t, ctx, log, cfg(c, "a"), jobs("a"))
	eventually(t, 5*time.Second, "a to lead", func() bool { h, _ := holder(t, c); return h == "a" })
	db := start(t, ctx, log, cfg(c, "b"), jobs("b"))

	// Several lease durations pass: b must never take over while a renews.
	time.Sleep(2 * tLease)
	mu.Lock()
	if running["a"] != 1 || running["b"] != 0 {
		t.Errorf("jobs started: %v, want only a once", running)
	}
	mu.Unlock()
	if h, _ := holder(t, c); h != "a" {
		t.Errorf("holder = %q, want a", h)
	}
	cancel()
	waitRun(t, da)
	waitRun(t, db)
}

func TestReleaseOnCancelHandsOverToStandby(t *testing.T) {
	c := fake.NewClientset()
	log, buf := newLog()
	ctxA, cancelA := context.WithCancel(context.Background())
	defer cancelA()
	ctxB, cancelB := context.WithCancel(context.Background())
	defer cancelB()

	// Wait for a's jobs, not just the Lease: the elector runs the start
	// callback in its own goroutine, and a cancel that lands before it has
	// run means the jobs (and the "released" log) never happen.
	var aStarted, bStarted atomic.Bool
	da := start(t, ctxA, log, cfg(c, "a"), func(jc context.Context) { aStarted.Store(true); <-jc.Done() })
	eventually(t, 5*time.Second, "a's jobs to start", aStarted.Load)
	db := start(t, ctxB, log, cfg(c, "b"), func(jc context.Context) { bStarted.Store(true); <-jc.Done() })

	cancelA()
	waitRun(t, da)
	// Run returned: the Lease must already be released (empty holder or b).
	if h, ok := holder(t, c); !ok || h == "a" {
		t.Fatalf("after a returned: holder=%q ok=%v, want released", h, ok)
	}
	// The standby takes over well before a crashed leader's LeaseDuration.
	eventually(t, tLease+tRetry*3, "b to lead", func() bool { return bStarted.Load() })
	eventually(t, 5*time.Second, "'leadership released' in the log", func() bool {
		return strings.Contains(buf.String(), "leadership released")
	})
	cancelB()
	waitRun(t, db)
}

func TestReleaseLeavesHolderEmptyWithoutStandby(t *testing.T) {
	c := fake.NewClientset()
	log, _ := newLog()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := start(t, ctx, log, cfg(c, "a"), func(jc context.Context) { <-jc.Done() })
	eventually(t, 5*time.Second, "a to lead", func() bool { h, _ := holder(t, c); return h == "a" })
	cancel()
	waitRun(t, d)
	if h, ok := holder(t, c); !ok || h != "" {
		t.Fatalf("holder=%q ok=%v, want an empty holderIdentity", h, ok)
	}
}

func TestJobsStopBeforeLeaseIsReleased(t *testing.T) {
	c := fake.NewClientset()
	log, _ := newLog()

	var mu sync.Mutex
	var events []string
	rec := func(e string) {
		mu.Lock()
		events = append(events, e)
		mu.Unlock()
	}
	// Record the moment the Lease update that clears the holder is issued.
	c.PrependReactor("update", "leases", func(a k8stesting.Action) (bool, runtime.Object, error) {
		ua, ok := a.(k8stesting.UpdateAction)
		if !ok {
			return false, nil, nil
		}
		if l := leaseOf(ua.GetObject()); l != nil && (l.Spec.HolderIdentity == nil || *l.Spec.HolderIdentity == "") {
			rec("lease released")
		}
		return false, nil, nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	d := start(t, ctx, log, cfg(c, "a"), func(jc context.Context) {
		close(started)
		<-jc.Done()
		rec("ctx cancelled")
		time.Sleep(300 * time.Millisecond)
		rec("jobs returned")
	})
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("jobs never started")
	}
	cancel()
	waitRun(t, d)

	mu.Lock()
	defer mu.Unlock()
	want := []string{"ctx cancelled", "jobs returned", "lease released"}
	if strings.Join(events, ",") != strings.Join(want, ",") {
		t.Fatalf("order = %v, want %v", events, want)
	}
}

func TestLosingTheLeaseCancelsJobsAndRecampaigns(t *testing.T) {
	c := fake.NewClientset()
	log, buf := newLog()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var calls atomic.Int32
	firstCancelled := make(chan struct{})
	d := start(t, ctx, log, cfg(c, "a"), func(jc context.Context) {
		n := calls.Add(1)
		<-jc.Done()
		if n == 1 {
			close(firstCancelled)
		}
	})
	eventually(t, 5*time.Second, "a to lead", func() bool { return calls.Load() == 1 })

	// Another holder takes the Lease over. The fake client does not enforce
	// resourceVersion, so model the apiserver's conflict: a's updates are
	// refused while "other" holds the Lease.
	var stolen atomic.Bool
	c.PrependReactor("update", "leases", func(a k8stesting.Action) (bool, runtime.Object, error) {
		ua, ok := a.(k8stesting.UpdateAction)
		if !ok || !stolen.Load() {
			return false, nil, nil
		}
		if l := leaseOf(ua.GetObject()); l != nil && l.Spec.HolderIdentity != nil && *l.Spec.HolderIdentity == "a" {
			return true, nil, apierrors.NewConflict(schema.GroupResource{Group: "coordination.k8s.io", Resource: "leases"}, "lease", errors.New("held by other"))
		}
		return false, nil, nil
	})
	l, err := c.CoordinationV1().Leases("ns").Get(ctx, "lease", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	other := "other"
	l.Spec.HolderIdentity = &other
	now := metav1.NewMicroTime(time.Now())
	l.Spec.RenewTime = &now
	if _, err := c.CoordinationV1().Leases("ns").Update(ctx, l, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	stolen.Store(true)

	select {
	case <-firstCancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("jobs ctx was not cancelled after the Lease was lost")
	}
	select {
	case err := <-d:
		t.Fatalf("Run returned after losing the Lease: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	// "other" goes away: the Lease is released and a campaigns again.
	stolen.Store(false)
	cur, err := c.CoordinationV1().Leases("ns").Get(ctx, "lease", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	empty := ""
	cur.Spec.HolderIdentity = &empty
	if _, err := c.CoordinationV1().Leases("ns").Update(ctx, cur, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	eventually(t, 10*time.Second, "jobs to run again", func() bool { return calls.Load() >= 2 })

	cancel()
	waitRun(t, d)
	for _, want := range []string{"leadership acquired", "leadership lost", "leadership released"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("log lacks %q:\n%s", want, buf.String())
		}
	}
}

func TestFailingRenewalsCancelJobsWithoutReturning(t *testing.T) {
	c := fake.NewClientset()
	log, buf := newLog()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var fail atomic.Bool
	c.PrependReactor("update", "leases", func(k8stesting.Action) (bool, runtime.Object, error) {
		if fail.Load() {
			return true, nil, errors.New("apiserver down")
		}
		return false, nil, nil
	})

	var calls atomic.Int32
	cancelled := make(chan struct{}, 4)
	d := start(t, ctx, log, cfg(c, "a"), func(jc context.Context) {
		calls.Add(1)
		<-jc.Done()
		cancelled <- struct{}{}
	})
	eventually(t, 5*time.Second, "a to lead", func() bool { return calls.Load() == 1 })
	fail.Store(true)
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("jobs ctx not cancelled when renewals fail")
	}
	select {
	case err := <-d:
		t.Fatalf("Run returned: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	if !strings.Contains(buf.String(), "leadership lost") {
		t.Errorf("log lacks 'leadership lost':\n%s", buf.String())
	}
	fail.Store(false)
	eventually(t, 10*time.Second, "jobs to run again", func() bool { return calls.Load() >= 2 })
	cancel()
	waitRun(t, d)
}

func TestCancelledBeforeAcquiringNeverRunsJobs(t *testing.T) {
	c := fake.NewClientset()
	log, _ := newLog()
	// Someone else holds a fresh Lease, so a cannot acquire it.
	other := "other"
	now := metav1.NewMicroTime(time.Now())
	dur := int32(60)
	if _, err := c.CoordinationV1().Leases("ns").Create(context.Background(), leaseFor("lease", "ns", other, now, dur), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	var called atomic.Bool
	ctx, cancel := context.WithCancel(context.Background())
	d := start(t, ctx, log, cfg(c, "a"), func(context.Context) { called.Store(true) })
	time.Sleep(3 * tRetry)
	cancel()
	waitRun(t, d)
	if called.Load() {
		t.Error("jobs ran without the Lease")
	}
	if h, _ := holder(t, c); h != other {
		t.Errorf("holder = %q, want %q untouched", h, other)
	}

	// An already-cancelled ctx returns at once, too.
	ctx2, cancel2 := context.WithCancel(context.Background())
	cancel2()
	if err := Run(ctx2, log, cfg(fake.NewClientset(), "a"), func(context.Context) { called.Store(true) }); err != nil {
		t.Fatal(err)
	}
	if called.Load() {
		t.Error("jobs ran with a cancelled ctx")
	}
}

func TestLogsAcquiredAndReleased(t *testing.T) {
	c := fake.NewClientset()
	log, buf := newLog()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var up atomic.Bool
	d := start(t, ctx, log, cfg(c, "a"), func(jc context.Context) { up.Store(true); <-jc.Done() })
	eventually(t, 5*time.Second, "a to lead", up.Load)
	cancel()
	waitRun(t, d)
	for _, want := range []string{"leadership acquired", "leadership released"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("log lacks %q:\n%s", want, buf.String())
		}
	}
	if strings.Contains(buf.String(), "leadership lost") {
		t.Errorf("clean shutdown logged 'leadership lost':\n%s", buf.String())
	}
}

func leaseOf(o runtime.Object) *coordv1.Lease {
	l, _ := o.(*coordv1.Lease)
	return l
}

func leaseFor(name, ns, holder string, renew metav1.MicroTime, secs int32) *coordv1.Lease {
	return &coordv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: coordv1.LeaseSpec{
			HolderIdentity: &holder, RenewTime: &renew, AcquireTime: &renew, LeaseDurationSeconds: &secs,
		},
	}
}
