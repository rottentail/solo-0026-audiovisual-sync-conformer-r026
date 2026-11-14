package service_test

import (
	"testing"
	"time"

	"github.com/rottentail/solo-0026-audiovisual-sync-conformer/internal/domain"
	"github.com/rottentail/solo-0026-audiovisual-sync-conformer/internal/service"
	"github.com/rottentail/solo-0026-audiovisual-sync-conformer/internal/storage"
)

func TestFailureIsolationAfterRestart(t *testing.T) {
	f := newFixture(t)
	finished := make(chan error, 1)
	go func() {
		_, err := conform(f.svc, "durable-good", 250)
		finished <- err
	}()
	deadline := time.Now().Add(3 * time.Second)
	for {
		job, err := f.svc.GetConform("durable-good")
		if err == nil && job.State == domain.ConformStateRendering {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("successful conform did not reach rendering")
		}
		time.Sleep(100 * time.Microsecond)
	}
	failed, err := conform(f.svc, "durable-low", 50)
	if err == nil || failed.Conform.State != domain.ConformStateFailed {
		t.Fatalf("low ceiling: %+v %v", failed, err)
	}
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	repo, err := storage.New(f.root)
	if err != nil {
		t.Fatal(err)
	}
	reopened := service.New(repo)
	checkBundle(t, reopened, "durable-good")
	failedAfterRestart, err := reopened.GetConform("durable-low")
	if err != nil || failedAfterRestart.State != domain.ConformStateFailed {
		t.Fatalf("failed conform was not retained: %+v %v", failedAfterRestart, err)
	}
}
