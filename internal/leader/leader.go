// Package leader gates the background writers of mail-mcp behind a Kubernetes
// Lease (coordination.k8s.io/v1).
//
// A RollingUpdate with overlap runs two pods on one node for a few seconds,
// both with the cache directory and the history volume mounted. Request
// handling is safe on both (SQLite WAL with a busy timeout, an O_APPEND
// history file); the periodic writers -- IMAP refreshers, the backfills, the
// senders recount, PDF text, retention -- are not meant to run twice, so only
// the holder of the Lease runs them. A pod that is not the leader keeps
// serving requests and keeps campaigning.
package leader

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

// Timing of the election. A crashed leader is replaced after LeaseDuration; a
// terminating one hands over at once (ReleaseOnCancel).
const (
	LeaseDuration = 15 * time.Second
	RenewDeadline = 10 * time.Second
	RetryPeriod   = 2 * time.Second
)

const namespaceFile = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

// Config configures Run.
type Config struct {
	// Mode is "auto" (election when running in a cluster), "true" (election,
	// an error outside a cluster) or "false" (always the leader).
	Mode string
	// Name of the Lease; Namespace it lives in (default: the pod's own).
	Name, Namespace string
	// Identity of this process (default: POD_NAME, then the hostname).
	Identity string

	// Client and the durations are for tests; zero values mean the in-cluster
	// client and the package constants.
	Client                              kubernetes.Interface
	LeaseDuration, RenewDeadline, Retry time.Duration
}

// Enabled resolves Mode. It reports whether to campaign and the client to
// campaign with (nil when not).
func (c *Config) resolve() (kubernetes.Interface, error) {
	mode := strings.ToLower(strings.TrimSpace(c.Mode))
	if mode == "" {
		mode = "auto"
	}
	switch mode {
	case "false", "off", "no", "0":
		return nil, nil
	case "auto", "true", "on", "yes", "1":
	default:
		return nil, fmt.Errorf("leader election: mode %q is not auto, true or false", c.Mode)
	}
	if c.Client != nil {
		return c.Client, nil
	}
	rc, err := rest.InClusterConfig()
	if err != nil {
		if mode == "auto" {
			return nil, nil
		}
		return nil, fmt.Errorf("leader election: not in a cluster: %w", err)
	}
	cl, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return nil, fmt.Errorf("leader election: client: %w", err)
	}
	return cl, nil
}

func (c *Config) fill() error {
	if c.Name == "" {
		c.Name = "mail-mcp"
	}
	if c.Namespace == "" {
		b, err := os.ReadFile(namespaceFile)
		if err != nil || strings.TrimSpace(string(b)) == "" {
			return errors.New("leader election: the lease namespace is unknown; set --lease-namespace or POD_NAMESPACE")
		}
		c.Namespace = strings.TrimSpace(string(b))
	}
	if c.Identity == "" {
		c.Identity = os.Getenv("POD_NAME")
	}
	if c.Identity == "" {
		h, err := os.Hostname()
		if err != nil || h == "" {
			return errors.New("leader election: no identity; set POD_NAME")
		}
		c.Identity = h
	}
	if c.LeaseDuration == 0 {
		c.LeaseDuration = LeaseDuration
	}
	if c.RenewDeadline == 0 {
		c.RenewDeadline = RenewDeadline
	}
	if c.Retry == 0 {
		c.Retry = RetryPeriod
	}
	return nil
}

// Run campaigns for the Lease until ctx ends. Each time this process becomes
// the leader it calls jobs with a context that ends when leadership is lost or
// ctx ends; jobs blocks until its work has stopped. Losing leadership is not
// fatal: Run campaigns again. On return -- ctx ended -- jobs has returned and
// the Lease has been released.
//
// With election off, jobs runs once, at once, for as long as ctx lives.
func Run(ctx context.Context, log *slog.Logger, cfg Config, jobs func(ctx context.Context)) error {
	client, err := cfg.resolve()
	if err != nil {
		return err
	}
	if client == nil {
		log.Info("leader election off; running the background jobs here", "mode", cfg.Mode)
		jobs(ctx)
		return nil
	}
	if err := cfg.fill(); err != nil {
		return err
	}
	log.Info("leader election on", "lease", cfg.Namespace+"/"+cfg.Name, "identity", cfg.Identity,
		"lease_duration", cfg.LeaseDuration.String(), "renew_deadline", cfg.RenewDeadline.String(), "retry_period", cfg.Retry.String())

	for ctx.Err() == nil {
		started := time.Now()
		if err := campaign(ctx, log, cfg, client, jobs); err != nil {
			return err
		}
		// Leadership lost (or never held): campaign again. The elector already
		// waits a retry period before acquiring; this only guards a loop that
		// returns at once.
		if ctx.Err() == nil && time.Since(started) < cfg.Retry {
			select {
			case <-ctx.Done():
			case <-time.After(cfg.Retry):
			}
		}
	}
	return nil
}

// campaign runs one elector until it loses the Lease or ctx ends.
func campaign(ctx context.Context, log *slog.Logger, cfg Config, client kubernetes.Interface, jobs func(context.Context)) error {
	lock := &resourcelock.LeaseLock{
		LeaseMeta:  metav1.ObjectMeta{Name: cfg.Name, Namespace: cfg.Namespace},
		Client:     client.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{Identity: cfg.Identity},
	}

	// The jobs stop before the Lease is released: the elector runs on ectx,
	// which is cancelled only after the jobs have returned.
	ectx, ecancel := context.WithCancel(context.WithoutCancel(ctx))
	defer ecancel()

	var (
		mu      sync.Mutex
		stopped bool
		jcancel context.CancelFunc
		done    chan struct{}
		led     bool
	)
	stopJobs := func() {
		mu.Lock()
		stopped = true
		c, d := jcancel, done
		mu.Unlock()
		if c != nil {
			c()
		}
		if d != nil {
			<-d
		}
	}

	le, err := leaderelection.NewLeaderElector(leaderelection.LeaderElectionConfig{
		Lock:            lock,
		LeaseDuration:   cfg.LeaseDuration,
		RenewDeadline:   cfg.RenewDeadline,
		RetryPeriod:     cfg.Retry,
		ReleaseOnCancel: true,
		Name:            cfg.Name,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(context.Context) {
				jctx, c := context.WithCancel(ctx)
				d := make(chan struct{})
				mu.Lock()
				if stopped {
					mu.Unlock()
					c()
					return
				}
				jcancel, done, led = c, d, true
				mu.Unlock()
				log.Info("leadership acquired", "identity", cfg.Identity, "lease", cfg.Namespace+"/"+cfg.Name)
				defer close(d)
				defer c()
				jobs(jctx)
			},
			OnStoppedLeading: func() {
				stopJobs()
				mu.Lock()
				was := led
				mu.Unlock()
				switch {
				case !was:
				case ctx.Err() != nil:
					log.Info("leadership released", "identity", cfg.Identity)
				default:
					log.Info("leadership lost", "identity", cfg.Identity)
				}
			},
			OnNewLeader: func(id string) {
				if id == cfg.Identity {
					return
				}
				log.Info("leader is another pod", "leader", id, "identity", cfg.Identity)
			},
		},
	})
	if err != nil {
		return fmt.Errorf("leader election: %w", err)
	}

	go func() {
		select {
		case <-ctx.Done():
			stopJobs()
			ecancel()
		case <-ectx.Done():
		}
	}()
	le.Run(ectx)
	return nil
}
