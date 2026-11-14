package service_test

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/rottentail/solo-0026-audiovisual-sync-conformer/internal/domain"
	"github.com/rottentail/solo-0026-audiovisual-sync-conformer/internal/media"
	"github.com/rottentail/solo-0026-audiovisual-sync-conformer/internal/service"
	"github.com/rottentail/solo-0026-audiovisual-sync-conformer/internal/storage"
)

type fixture struct {
	root string
	svc  *service.Service
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	root := t.TempDir()
	repo, err := storage.New(root)
	if err != nil {
		t.Fatal(err)
	}
	audio, video, err := media.CalibrationPair()
	if err != nil {
		t.Fatal(err)
	}
	clip, err := media.InspectVideo(video)
	if err != nil {
		t.Fatal(err)
	}
	for len(clip.Frames) < 800 {
		clip.Frames = append(clip.Frames, clip.Frames[0])
	}
	video, err = media.EncodeY4M(clip.Info, clip.Frames)
	if err != nil {
		t.Fatal(err)
	}
	svc := service.New(repo)
	take, err := svc.CreateTake(domain.CreateTakeInput{
		TakeID: "take-siblings", Name: "two editors",
		AudioBase64: encodeBase64(audio), VideoBase64: encodeBase64(video),
	})
	if err != nil || !take.Created {
		t.Fatalf("admit: %+v %v", take, err)
	}
	analysis, err := svc.Analyze(take.Take.ID, domain.CreateAnalysisInput{
		AnalysisID: "sync-siblings", MinConfidence: 0.55, MaxOffsetMS: 1000,
	})
	if err != nil || analysis.Analysis.State != domain.AnalysisStateReady {
		t.Fatalf("analyze: %+v %v", analysis, err)
	}
	return fixture{root: root, svc: svc}
}

func encodeBase64(data []byte) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	out := make([]byte, 0, (len(data)+2)/3*4)
	for i := 0; i < len(data); i += 3 {
		n := int(data[i]) << 16
		if i+1 < len(data) {
			n |= int(data[i+1]) << 8
		}
		if i+2 < len(data) {
			n |= int(data[i+2])
		}
		out = append(out, alphabet[(n>>18)&63], alphabet[(n>>12)&63])
		if i+1 < len(data) {
			out = append(out, alphabet[(n>>6)&63])
		} else {
			out = append(out, '=')
		}
		if i+2 < len(data) {
			out = append(out, alphabet[n&63])
		} else {
			out = append(out, '=')
		}
	}
	return string(out)
}

func conform(svc *service.Service, id string, ceiling int) (domain.ConformResult, error) {
	return svc.CreateConform("sync-siblings", domain.CreateConformInput{
		ConformID: id, MaxCorrectionMS: ceiling,
	})
}

func checkBundle(t *testing.T, svc *service.Service, id string) {
	t.Helper()
	bundle, receipt, err := svc.ReadConformBundle(id)
	if err != nil {
		t.Fatalf("%s read: %v", id, err)
	}
	if receipt.ConformID != id {
		t.Fatalf("%s sidecar belongs to %s", id, receipt.ConformID)
	}
	archive, err := zip.NewReader(bytes.NewReader(bundle), int64(len(bundle)))
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range archive.File {
		if item.Name != "conform-receipt.json" {
			continue
		}
		reader, err := item.Open()
		if err != nil {
			t.Fatal(err)
		}
		payload, err := io.ReadAll(reader)
		reader.Close()
		if err != nil {
			t.Fatal(err)
		}
		var embedded domain.ConformReceipt
		if err := json.Unmarshal(payload, &embedded); err != nil {
			t.Fatal(err)
		}
		if embedded.ConformID != id {
			t.Fatalf("%s archive belongs to %s", id, embedded.ConformID)
		}
		return
	}
	t.Fatal("archive receipt missing")
}

func runBoth(t *testing.T, f fixture, prefix string) [2]string {
	t.Helper()
	ids := [2]string{prefix + "-a", prefix + "-b"}
	var results [2]domain.ConformResult
	var errors [2]error
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range ids {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i], errors[i] = conform(f.svc, ids[i], 250)
		}(i)
	}
	close(start)
	wg.Wait()
	for i := range ids {
		if errors[i] != nil || results[i].Conform.State != domain.ConformStateCompleted {
			t.Fatalf("%s conform: %+v %v", ids[i], results[i], errors[i])
		}
	}
	return ids
}

func TestSiblingConformsBoth(t *testing.T) {
	f := newFixture(t)
	ids := runBoth(t, f, "both")
	for _, id := range ids {
		checkBundle(t, f.svc, id)
	}
}

func TestSiblingConformsMixed(t *testing.T) {
	f := newFixture(t)
	success := make(chan error, 1)
	go func() {
		_, err := conform(f.svc, "mixed-good", 250)
		success <- err
	}()
	deadline := time.Now().Add(3 * time.Second)
	for {
		record, err := f.svc.GetConform("mixed-good")
		if err == nil && record.State == domain.ConformStateRendering {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("successful conform did not reach rendering")
		}
		time.Sleep(time.Microsecond * 100)
	}
	failed, err := conform(f.svc, "mixed-low", 50)
	if err == nil || failed.Conform.State != domain.ConformStateFailed {
		t.Fatalf("low ceiling: %+v %v", failed, err)
	}
	if err := <-success; err != nil {
		t.Fatal(err)
	}
	checkBundle(t, f.svc, "mixed-good")
}

func TestSiblingConformsRestart(t *testing.T) {
	f := newFixture(t)
	ids := runBoth(t, f, "restart")
	repo, err := storage.New(f.root)
	if err != nil {
		t.Fatal(err)
	}
	reopened := service.New(repo)
	for _, id := range ids {
		checkBundle(t, reopened, id)
	}
}

func TestConformRetry(t *testing.T) {
	f := newFixture(t)
	first, err := conform(f.svc, "retry-one", 250)
	if err != nil {
		t.Fatal(err)
	}
	again, err := conform(f.svc, "retry-one", 250)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Created || again.Created || first.Conform.BundleDigest != again.Conform.BundleDigest {
		t.Fatal(fmt.Sprintf("retry mismatch: first=%+v again=%+v", first, again))
	}
	checkBundle(t, f.svc, "retry-one")
}

func TestLegacyConformRead(t *testing.T) {
	root := os.Getenv("PROBE_DATA_ROOT")
	if root == "" {
		t.Skip("PROBE_DATA_ROOT is required")
	}
	checkLegacyRoot(t, root, false)
}

func checkLegacyRoot(t *testing.T, root string, create bool) {
	t.Helper()
	if create {
		repo, err := storage.New(root)
		if err != nil {
			t.Fatal(err)
		}
		audio, video, err := media.CalibrationPair()
		if err != nil {
			t.Fatal(err)
		}
		svc := service.New(repo)
		_, err = svc.CreateTake(domain.CreateTakeInput{
			TakeID: "legacy-take", Name: "legacy review",
			AudioBase64: encodeBase64(audio), VideoBase64: encodeBase64(video),
		})
		if err != nil {
			t.Fatal(err)
		}
		_, err = svc.Analyze("legacy-take", domain.CreateAnalysisInput{
			AnalysisID: "legacy-analysis", MinConfidence: 0.55, MaxOffsetMS: 1000,
		})
		if err != nil {
			t.Fatal(err)
		}
		_, err = svc.CreateConform("legacy-analysis", domain.CreateConformInput{
			ConformID: "legacy-completed", MaxCorrectionMS: 250,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	repo, err := storage.New(root)
	if err != nil {
		t.Fatal(err)
	}
	checkBundle(t, service.New(repo), "legacy-completed")
}
