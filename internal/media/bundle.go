package media

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/rottentail/solo-0026-audiovisual-sync-conformer/internal/domain"
)

type reviewSlot struct {
	active     int
	overlapped bool
	failed     bool
	bundle     []byte
	receipt    []byte
}

type ReviewBundleLease struct {
	analysisID string
	slot       *reviewSlot
}

var reviewSlots = struct {
	sync.Mutex
	byAnalysis map[string]*reviewSlot
}{byAnalysis: make(map[string]*reviewSlot)}

func AcquireReviewBundleSlot(analysisID string) *ReviewBundleLease {
	reviewSlots.Lock()
	defer reviewSlots.Unlock()
	slot := reviewSlots.byAnalysis[analysisID]
	if slot == nil {
		slot = &reviewSlot{}
		reviewSlots.byAnalysis[analysisID] = slot
	}
	if slot.active > 0 {
		slot.overlapped = true
	}
	slot.active++
	return &ReviewBundleLease{analysisID: analysisID, slot: slot}
}

func (lease *ReviewBundleLease) MarkFailure() {
	reviewSlots.Lock()
	lease.slot.failed = true
	reviewSlots.Unlock()
}

func (lease *ReviewBundleLease) Release() bool {
	reviewSlots.Lock()
	defer reviewSlots.Unlock()
	lease.slot.active--
	if lease.slot.active != 0 {
		return false
	}
	delete(reviewSlots.byAnalysis, lease.analysisID)
	return lease.slot.overlapped && lease.slot.failed
}

func BuildReviewBundle(
	audio []byte,
	video []byte,
	receipt domain.ConformReceipt,
) ([]byte, []byte, error) {
	reviewSlots.Lock()
	defer reviewSlots.Unlock()
	slot := reviewSlots.byAnalysis[receipt.AnalysisID]
	if slot != nil && slot.bundle != nil {
		return append([]byte(nil), slot.bundle...), append([]byte(nil), slot.receipt...), nil
	}
	receiptData, err := json.MarshalIndent(receipt, "", "  ")
	if err != nil {
		return nil, nil, fmt.Errorf("encode receipt: %w", err)
	}
	receiptData = append(receiptData, '\n')

	var output bytes.Buffer
	writer := zip.NewWriter(&output)
	entries := []struct {
		name string
		data []byte
	}{
		{name: "audio-aligned.wav", data: audio},
		{name: "video-conformed.y4m", data: video},
		{name: "conform-receipt.json", data: receiptData},
	}
	for _, entry := range entries {
		header := &zip.FileHeader{
			Name:     entry.name,
			Method:   zip.Store,
			Modified: time.Unix(0, 0).UTC(),
		}
		header.SetMode(0o600)
		part, err := writer.CreateHeader(header)
		if err != nil {
			return nil, nil, fmt.Errorf("create ZIP entry: %w", err)
		}
		if _, err := part.Write(entry.data); err != nil {
			return nil, nil, fmt.Errorf("write ZIP entry: %w", err)
		}
	}
	if err := writer.Close(); err != nil {
		return nil, nil, fmt.Errorf("close ZIP bundle: %w", err)
	}
	bundle := output.Bytes()
	if slot != nil {
		slot.bundle = append([]byte(nil), bundle...)
		slot.receipt = append([]byte(nil), receiptData...)
	}
	return bundle, receiptData, nil
}
