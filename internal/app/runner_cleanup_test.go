package app

import (
	"context"
	"testing"
	"time"
)

type runnerCleanupTestService struct {
	cleanup func()
}

func (s runnerCleanupTestService) Name() string { return "test-cleanup" }

func (s runnerCleanupTestService) Start(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

func (s runnerCleanupTestService) Stop(context.Context) error {
	s.cleanup()
	return nil
}

func TestRunnerStopsCleanupService(t *testing.T) {
	cleaned := false
	runner := NewRunner(runnerCleanupTestService{
		cleanup: func() {
			cleaned = true
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := runner.Run(ctx, time.Second, nil); err != nil {
		t.Fatalf("run cleanup service: %v", err)
	}
	if !cleaned {
		t.Fatal("cleanup service was not stopped")
	}
}
