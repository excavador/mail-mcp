package leader

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"
)

func TestAutoModeFallsBackOnlyOnErrNotInCluster(t *testing.T) {
	// Outside a cluster: ErrNotInCluster, auto means "off".
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")
	if _, err := rest.InClusterConfig(); !errors.Is(err, rest.ErrNotInCluster) {
		t.Fatalf("precondition: InClusterConfig err = %v", err)
	}
	e, err := New(Config{Mode: "auto"})
	if err != nil || e.On() {
		t.Fatalf("auto outside a cluster: err=%v on=%v", err, e != nil && e.On())
	}

	// Environment says "in a cluster" but the token is unreadable: a different
	// error, which auto must not swallow.
	if _, err := os.Stat("/var/run/secrets/kubernetes.io/serviceaccount/token"); err == nil {
		t.Skip("running inside a pod with a service account token")
	}
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.0.0.1")
	t.Setenv("KUBERNETES_SERVICE_PORT", "443")
	if _, err := rest.InClusterConfig(); err == nil || errors.Is(err, rest.ErrNotInCluster) {
		t.Fatalf("precondition: InClusterConfig err = %v", err)
	}
	for _, mode := range []string{"auto", "true"} {
		if e, err := New(Config{Mode: mode}); err == nil {
			t.Errorf("mode %s with a broken in-cluster config: no error (elector %+v)", mode, e)
		}
	}
}

func TestNewValidatesAtStartup(t *testing.T) {
	if _, err := New(Config{Mode: "bogus"}); err == nil {
		t.Error("invalid mode accepted")
	}
	// A client but no namespace anywhere: the error is at New, not at Run.
	if _, err := os.Stat(namespaceFile); err == nil {
		t.Skip("running inside a pod: the namespace file exists")
	}
	if _, err := New(Config{Mode: "true", Client: fake.NewClientset()}); err == nil || !strings.Contains(err.Error(), "namespace") {
		t.Errorf("missing namespace: err = %v", err)
	}
	e, err := New(Config{Mode: "true", Client: fake.NewClientset(), Namespace: "ns", Identity: "x"})
	if err != nil || !e.On() {
		t.Fatalf("valid config: %v", err)
	}
	e, err = New(Config{Mode: "false"})
	if err != nil || e.On() {
		t.Fatalf("mode false: %v", err)
	}
}

func TestJobsCtxCancelledAtRenewDeadlineBeforeAnyRelease(t *testing.T) {
	c := fake.NewClientset()
	log, _ := newLog()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	failUpdates := make(chan struct{})
	started := make(chan struct{})
	c.PrependReactor("update", "leases", func(k8stesting.Action) (bool, runtime.Object, error) {
		select {
		case <-failUpdates:
			return true, nil, errors.New("apiserver down")
		default:
			return false, nil, nil
		}
	})
	e, err := New(cfg(c, "a"))
	if err != nil {
		t.Fatal(err)
	}
	cancelledAt := make(chan time.Time, 1)
	done := make(chan error, 1)
	go func() {
		done <- e.Run(ctx, log, func(jc context.Context) {
			select {
			case <-started:
			default:
				close(started)
			}
			<-jc.Done()
			select {
			case cancelledAt <- time.Now():
			default:
			}
		})
	}()
	<-started
	t0 := time.Now()
	close(failUpdates)
	select {
	case at := <-cancelledAt:
		// Renewal gives up at RenewDeadline; the jobs stop then, not at
		// LeaseDuration or at process shutdown.
		if d := at.Sub(t0); d > tRenew+time.Second {
			t.Errorf("jobs stopped %v after renewals began failing, want about %v", d, tRenew)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("jobs ctx not cancelled while the Lease cannot be renewed")
	}
	cancel()
	waitRun(t, done)
}
