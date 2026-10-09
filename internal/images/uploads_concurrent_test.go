package images

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestFinalizeUpload_ConcurrentSameSession is the regression test for the
// duplicate-finalize race: two concurrent FinalizeUpload calls on one session
// used to both pass the "all chunks present" check and interleave on the same
// assembled/final paths. Now claimFinalize admits exactly one; the loser gets
// ErrSessionBusy. Mirrors bindings.TestUpsert_ConcurrentAutoAllocate.
func TestFinalizeUpload_ConcurrentSameSession(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	body := bytes.Repeat([]byte{0xCD}, 10*1024)
	in := uploadBytes("race.qcow2", body, 5*1024)
	sess, err := s.CreateUpload(ctx, in)
	if err != nil {
		t.Fatalf("CreateUpload: %v", err)
	}
	for i, c := range [][]byte{body[:5120], body[5120:]} {
		if _, err := s.WriteChunk(ctx, sess.ID, i+1, bytes.NewReader(c), sha256Hex(c)); err != nil {
			t.Fatalf("WriteChunk %d: %v", i+1, err)
		}
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make([]*FinalizeUploadResult, 2)
	errs := make([]error, 2)
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			res, err := s.FinalizeUpload(ctx, sess.ID, extractor{format: "qcow2"})
			results[i], errs[i] = res, err
		}()
	}
	close(start)
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("concurrent FinalizeUpload did not finish within 30s (deadlock?)")
	}

	var wins int
	for i, err := range errs {
		switch {
		case err == nil:
			wins++
			if results[i].FinalSHA != in.ExpectedSHA256 {
				t.Errorf("winner[%d] FinalSHA %q want %q", i, results[i].FinalSHA, in.ExpectedSHA256)
			}
		case errors.Is(err, ErrSessionBusy):
			// expected loser
		default:
			t.Errorf("finalize[%d]: unexpected error: %v", i, err)
		}
	}
	if wins != 1 {
		t.Fatalf("expected exactly 1 winning finalize, got %d (errs: %v / %v)", wins, errs[0], errs[1])
	}

	// The winner's file must be intact and the session row gone.
	data, err := os.ReadFile(s.FinalPath("race.qcow2", "qcow2", in.ExpectedSHA256))
	if err != nil {
		t.Fatalf("read final: %v", err)
	}
	if !bytes.Equal(data, body) {
		t.Error("final file bytes corrupted after concurrent finalize")
	}
	if _, err := s.GetUpload(ctx, sess.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("session row should be deleted, got err=%v", err)
	}
}

// TestWriteChunk_RejectedDuringFinalize: once a session is claimed for
// finalizing, chunk writes must fail rather than interleave with the
// concatenation.
func TestWriteChunk_RejectedDuringFinalize(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// Session with 3 chunks but only chunk 1 uploaded; finalize will claim
	// the session then fail on the missing chunk.
	body := bytes.Repeat([]byte{0xEE}, 300)
	in := uploadBytes("busy.qcow2", body, 100)
	sess, err := s.CreateUpload(ctx, in)
	if err != nil {
		t.Fatalf("CreateUpload: %v", err)
	}
	if _, err := s.WriteChunk(ctx, sess.ID, 1, bytes.NewReader(body[:100]), sha256Hex(body[:100])); err != nil {
		t.Fatalf("WriteChunk 1: %v", err)
	}

	if _, err := s.FinalizeUpload(ctx, sess.ID, extractor{format: "qcow2"}); !errors.Is(err, ErrChunkMissing) {
		t.Fatalf("expected ErrChunkMissing from incomplete finalize, got %v", err)
	}

	// A failed finalize releases the claim: chunk writes work again, and a
	// completed retry succeeds.
	if _, err := s.WriteChunk(ctx, sess.ID, 2, bytes.NewReader(body[100:200]), sha256Hex(body[100:200])); err != nil {
		t.Fatalf("WriteChunk 2 after failed finalize: %v", err)
	}
	if _, err := s.WriteChunk(ctx, sess.ID, 3, bytes.NewReader(body[200:]), sha256Hex(body[200:])); err != nil {
		t.Fatalf("WriteChunk 3 after failed finalize: %v", err)
	}
	if _, err := s.FinalizeUpload(ctx, sess.ID, extractor{format: "qcow2"}); err != nil {
		t.Fatalf("retry FinalizeUpload: %v", err)
	}

	// ...but while a claim is HELD, writes are rejected. Claim manually and
	// check the rejection path without racing a real finalize.
	body2 := bytes.Repeat([]byte{0xDD}, 200)
	in2 := uploadBytes("held.qcow2", body2, 100)
	sess2, err := s.CreateUpload(ctx, in2)
	if err != nil {
		t.Fatalf("CreateUpload 2: %v", err)
	}
	if err := s.claimFinalize(ctx, sess2.ID); err != nil {
		t.Fatalf("claimFinalize: %v", err)
	}
	_, err = s.WriteChunk(ctx, sess2.ID, 1, bytes.NewReader(body2[:100]), sha256Hex(body2[:100]))
	if !errors.Is(err, ErrSessionBusy) {
		t.Errorf("WriteChunk during finalizing: got %v want ErrSessionBusy", err)
	}
}

// TestFilenameUniqueIndex: the DB-level guard — two different sessions
// finalizing the same upload name must end up with two distinct catalog
// filenames (the loser gets the -sha8 suffix), never one file silently
// overwriting the other.
func TestFilenameUniqueIndex(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	mk := func(fill byte) ([]byte, UploadSession) {
		t.Helper()
		body := bytes.Repeat([]byte{fill}, 128)
		sess, err := s.CreateUpload(ctx, uploadBytes("dup.qcow2", body, 128))
		if err != nil {
			t.Fatalf("CreateUpload: %v", err)
		}
		if _, err := s.WriteChunk(ctx, sess.ID, 1, bytes.NewReader(body), sha256Hex(body)); err != nil {
			t.Fatalf("WriteChunk: %v", err)
		}
		return body, *sess
	}

	body1, sess1 := mk(0x11)
	if _, err := s.FinalizeUpload(ctx, sess1.ID, extractor{format: "qcow2"}); err != nil {
		t.Fatalf("finalize 1: %v", err)
	}
	body2, sess2 := mk(0x22)
	res2, err := s.FinalizeUpload(ctx, sess2.ID, extractor{format: "qcow2"})
	if err != nil {
		t.Fatalf("finalize 2: %v", err)
	}

	if res2.Image.Filename == "dup.qcow2" {
		t.Errorf("second image must get a suffixed filename, got %q", res2.Image.Filename)
	}
	// Both files on disk with their own bytes.
	p1 := s.FinalPath("dup.qcow2", "qcow2", sha256Hex(body1))
	if b, err := os.ReadFile(p1); err != nil || !bytes.Equal(b, body1) {
		t.Errorf("file 1 corrupted: err=%v", err)
	}
	p2 := filepath.Join(s.dir, res2.Image.Filename)
	if b, err := os.ReadFile(p2); err != nil || !bytes.Equal(b, body2) {
		t.Errorf("file 2 corrupted: err=%v", err)
	}
}
