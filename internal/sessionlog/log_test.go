package sessionlog

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"sync"
	"testing"

	"github.com/open-mcp-ai/termcp/internal/encryptedfs"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

const logIdentity = "sessions/session-a/shell-a/events"

func newTestLog(t *testing.T) (*Log, string, encryptedfs.Key) {
	t.Helper()
	key, err := encryptedfs.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "events.enc")
	l, err := Create(path, key, logIdentity)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l, path, key
}

func assertOutput(t *testing.T, l *Log, want []byte) {
	t.Helper()
	if size, err := l.Size(); err != nil || size != int64(len(want)) {
		t.Fatalf("output size=%d err=%v", size, err)
	}
	for offset := 0; offset <= len(want)+1; offset++ {
		length := min(53, len(want)+1)
		buf := make([]byte, length)
		n, err := l.ReadAt(buf, int64(offset))
		start := min(offset, len(want))
		expected := want[start:min(start+length, len(want))]
		if n != len(expected) || !bytes.Equal(buf[:n], expected) {
			t.Fatalf("output byte offset=%d returned different bytes", offset)
		}
		if length > len(expected) && !errors.Is(err, io.EOF) || length == len(expected) && err != nil {
			t.Fatalf("offset=%d error=%v", offset, err)
		}
	}
}

func TestOutputProjectionReplayAndTimeLookup(t *testing.T) {
	l, path, key := newTestLog(t)
	if err := l.RecordResize(900, 120, 40); err != nil {
		t.Fatal(err)
	}
	if err := l.RecordMark(api.LogMark{Status: api.LogApprovalRequest, Time: 901}); err != nil {
		t.Fatal(err)
	}
	if size, err := l.Size(); err != nil || size != 0 {
		t.Fatal("non-output events advanced the public byte offset")
	}
	var want []byte
	type timedOutput struct {
		time   int64
		offset int
	}
	var outputs []timedOutput
	for i := 0; i < 150; i++ {
		timestamp := int64(1000 + i*10)
		if i >= 50 && i < 100 {
			timestamp -= 900 // wall clock goes backwards across checkpoint blocks
		}
		data := []byte(fmt.Sprintf("%03d\x1b[31m\xff\x00\r\n", i))
		stream := Stream(i % 2)
		offset, err := l.RecordOutput(timestamp, data, stream)
		if err != nil || offset != int64(len(want)) {
			t.Fatalf("append output offset=%d err=%v", offset, err)
		}
		outputs = append(outputs, timedOutput{timestamp, len(want)})
		want = append(want, data...)
		// Existing marks are written after output and may refer to its start.
		if err := l.RecordMark(api.LogMark{Status: api.LogAIInput, Time: timestamp, Offset: offset}); err != nil {
			t.Fatal(err)
		}
		if i%3 == 0 {
			if err := l.RecordResize(timestamp, uint16(80+i), 24); err != nil {
				t.Fatal(err)
			}
		}
	}
	assertOutput(t, l, want)
	for _, timestamp := range []int64{0, 1000, 1099, 1490, 1500, 1990, 2000, 2485, 2490, 2491} {
		first := len(outputs) // timestamps are intentionally unsorted
		for i, out := range outputs {
			if out.time >= timestamp {
				first = i
				break
			}
		}
		got, err := l.ReadByTime(timestamp, 37)
		if first == len(outputs) {
			if !errors.Is(err, io.EOF) {
				t.Fatalf("time beyond history = %v", err)
			}
			continue
		}
		offset := outputs[first].offset
		if err != nil || !bytes.Equal(got, want[offset:min(offset+37, len(want))]) {
			t.Fatalf("time lookup failed for timestamp=%d: %v", timestamp, err)
		}
	}
	var events []Event
	for sequence := uint64(0); sequence < l.records; sequence++ {
		event, err := l.ReadEvent(sequence)
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	if len(l.checkpoints) != (len(events)+indexStride-1)/indexStride {
		t.Fatal("history index is not sparse")
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path, key, logIdentity)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	assertOutput(t, reopened, want)
	for _, want := range events {
		got, err := reopened.ReadEvent(want.Sequence)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("event %d changed after restart: %v", want.Sequence, err)
		}
	}
}

func TestIndexLossStalenessAndCorruptionDoNotChangeHistory(t *testing.T) {
	l, path, key := newTestLog(t)
	indexKey, err := encryptedfs.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	indexPath := filepath.Join(filepath.Dir(path), "events.idx.enc")
	for i := 0; i < 130; i++ {
		if _, err := l.RecordOutput(int64(1000+i), []byte("private history marker\n"), Stdout); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.SaveIndex(indexPath, indexKey); err != nil {
		t.Fatal(err)
	}
	if err := l.ValidateIndex(indexPath, indexKey); err != nil {
		t.Fatal(err)
	}
	if _, err := l.RecordOutput(2000, []byte("tail"), Stderr); err != nil {
		t.Fatal(err)
	}
	if err := l.ValidateIndex(indexPath, indexKey); !errors.Is(err, ErrIndex) {
		t.Fatalf("stale index accepted: %v", err)
	}
	if err := l.SaveIndex(indexPath, indexKey); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)-1] ^= 1
	if err := os.WriteFile(indexPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := l.ValidateIndex(indexPath, indexKey); !errors.Is(err, ErrIndex) || !errors.Is(err, encryptedfs.ErrAuthentication) {
		t.Fatalf("corrupt encrypted index accepted: %v", err)
	}
	if err := os.Remove(indexPath); err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenAppend(path, key, logIdentity)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	want := append(bytes.Repeat([]byte("private history marker\n"), 130), []byte("tail")...)
	assertOutput(t, reopened, want)
	if err := reopened.SaveIndex(indexPath, indexKey); err != nil {
		t.Fatal(err)
	}
	if err := reopened.ValidateIndex(indexPath, indexKey); err != nil {
		t.Fatal("reconstructed index disagreed with authenticated history")
	}
}

func TestInvalidEventsAndAuthenticatedOffsetDiscontinuityAreRejected(t *testing.T) {
	l, path, key := newTestLog(t)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, err := range []error{
		l.RecordResize(1, 0, 24),
		l.RecordResize(-1, 80, 24),
		l.RecordMark(api.LogMark{Status: api.LogAIInput, Time: 1, Offset: 1}),
		l.RecordMark(api.LogMark{Time: 1}),
		l.RecordMark(api.LogMark{Status: api.LogStatus(string([]byte{0xff})), Time: 1}),
	} {
		if !errors.Is(err, ErrEvent) {
			t.Fatalf("invalid event accepted: %v", err)
		}
	}
	for _, tc := range []struct {
		time   int64
		data   []byte
		stream Stream
	}{
		{-1, []byte("invalid timestamp"), Stdout},
		{1, make([]byte, MaxOutput+1), Stdout},
		{1, []byte("invalid stream"), Stream(2)},
	} {
		if _, err := l.RecordOutput(tc.time, tc.data, tc.stream); !errors.Is(err, ErrEvent) {
			t.Fatalf("invalid output accepted: %v", err)
		}
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("invalid append changed authoritative history")
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	// Valid AEAD cannot substitute for validating the business event schema.
	f, err := encryptedfs.OpenAppend(path, key, logIdentity)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := encode(Event{TimestampMS: 1, Type: Output, Data: []byte("valid AEAD, invalid output offset")})
	if err != nil {
		t.Fatal(err)
	}
	binary.LittleEndian.PutUint64(encoded[12:20], 1)
	if _, err := f.Append(encoded); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	before, _ = os.ReadFile(path)
	if _, err := OpenAppend(path, key, logIdentity); !errors.Is(err, ErrEvent) {
		t.Fatalf("discontinuous event offset accepted: %v", err)
	}
	after, err = os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("invalid event scan repaired authoritative history")
	}
}

func TestIndexCannotReplaceAuthoritativeLogOrItsAlias(t *testing.T) {
	l, path, _ := newTestLog(t)
	if _, err := l.RecordOutput(1, []byte("authoritative output"), Stdout); err != nil {
		t.Fatal(err)
	}
	indexKey, err := encryptedfs.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.SaveIndex(path, indexKey); !errors.Is(err, ErrIndex) {
		t.Fatalf("index overwrite of authoritative path accepted: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("saving an index replaced the authoritative log")
	}
	t.Run("hard link alias", func(t *testing.T) {
		alias := filepath.Join(filepath.Dir(path), "alias.enc")
		if err := os.Link(path, alias); err != nil {
			t.Skip("test filesystem does not support hard links")
		}
		if err := l.SaveIndex(alias, indexKey); !errors.Is(err, ErrIndex) {
			t.Fatalf("index overwrite of authoritative alias accepted: %v", err)
		}
		after, err := os.ReadFile(alias)
		if err != nil || !bytes.Equal(before, after) {
			t.Fatal("saving an index replaced an authoritative alias")
		}
	})
}

func TestIndexIsBoundToTheExactLogGeneration(t *testing.T) {
	l, path, key := newTestLog(t)
	indexKey, err := encryptedfs.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	indexPath := filepath.Join(filepath.Dir(path), "events.idx.enc")
	if _, err := l.RecordOutput(1, []byte("identical output"), Stdout); err != nil {
		t.Fatal(err)
	}
	if err := l.SaveIndex(indexPath, indexKey); err != nil {
		t.Fatal(err)
	}
	other, err := Create(filepath.Join(filepath.Dir(path), "other.enc"), key, logIdentity)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if _, err := other.RecordOutput(1, []byte("identical output"), Stdout); err != nil {
		t.Fatal(err)
	}
	if err := other.ValidateIndex(indexPath, indexKey); !errors.Is(err, ErrIndex) {
		t.Fatalf("foreign generation's authenticated index accepted: %v", err)
	}
	assertOutput(t, other, []byte("identical output"))
}

func TestConcurrentOutputAndMetadataKeepOneByteCoordinateSystem(t *testing.T) {
	l, _, _ := newTestLog(t)
	type result struct {
		offset int64
		data   []byte
	}
	results := make(chan result, 160)
	errs := make(chan error, 4)
	var workers sync.WaitGroup
	for worker := 0; worker < 4; worker++ {
		workers.Add(1)
		go func(worker int) {
			defer workers.Done()
			for i := 0; i < 40; i++ {
				data := []byte(fmt.Sprintf("%d/%d\n", worker, i))
				offset, err := l.RecordOutput(int64(i), data, Stream(worker%2))
				if err == nil {
					err = l.RecordResize(int64(i), 80, 24)
				}
				if err == nil {
					err = l.RecordMark(api.LogMark{Status: api.LogApprovalGranted, Time: int64(i), Offset: offset})
				}
				if err != nil {
					errs <- err
					return
				}
				results <- result{offset, data}
			}
		}(worker)
	}
	workers.Wait()
	close(results)
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	var writes []result
	for r := range results {
		writes = append(writes, r)
	}
	sort.Slice(writes, func(i, j int) bool { return writes[i].offset < writes[j].offset })
	var want []byte
	for _, write := range writes {
		if write.offset != int64(len(want)) {
			t.Fatal("metadata altered the output coordinate system")
		}
		want = append(want, write.data...)
	}
	if len(writes) != 160 {
		t.Fatal("lost acknowledged concurrent outputs")
	}
	assertOutput(t, l, want)
}
