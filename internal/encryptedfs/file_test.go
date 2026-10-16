package encryptedfs

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"sync"
	"testing"
)

const testIdentity = "sessions/session-a/shell-a/events"

func testFile(t *testing.T) (*File, string, Key) {
	t.Helper()
	key, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "events.enc")
	f, err := Create(path, key, testIdentity)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f, path, key
}

func TestFilePlaintextOffsetsLiveReadsAndSeek(t *testing.T) {
	f, path, key := testFile(t)
	chunks := [][]byte{[]byte("private output marker\r\n"), {0xff, 0, '\n'}, []byte("\x1b[31mcolor\x1b[0m\ntrailing")}
	var want []byte
	for _, chunk := range chunks {
		offset, err := f.Append(chunk)
		if err != nil || offset != int64(len(want)) {
			t.Fatalf("append offset=%d err=%v", offset, err)
		}
		want = append(want, chunk...)
	}
	if size, err := f.Size(); err != nil || size != int64(len(want)) {
		t.Fatalf("logical size=%d err=%v", size, err)
	}
	// A separate reader must work before the writer closes, including on Windows.
	reader, err := Open(path, key, testIdentity)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	for offset := 0; offset <= len(want)+1; offset++ {
		for _, length := range []int{0, 1, 3, 17, len(want) + 1} {
			buf := make([]byte, length)
			n, err := reader.ReadAt(buf, int64(offset))
			start := min(offset, len(want))
			expected := want[start:min(start+length, len(want))]
			if n != len(expected) || !bytes.Equal(buf[:n], expected) {
				t.Fatalf("ReadAt offset=%d length=%d returned wrong bytes", offset, length)
			}
			if length > len(expected) && !errors.Is(err, io.EOF) || length <= len(expected) && err != nil {
				t.Fatalf("ReadAt offset=%d length=%d error=%v", offset, length, err)
			}
		}
	}
	if _, err := reader.Append([]byte("refused")); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("read-only append = %v", err)
	}
	if _, err := reader.Seek(-3, io.SeekEnd); err != nil {
		t.Fatal(err)
	}
	tail, err := io.ReadAll(reader)
	if err != nil || !bytes.Equal(tail, want[len(want)-3:]) {
		t.Fatal("Seek/Read did not use plaintext positions")
	}
	if _, err := reader.Seek(math.MaxInt64, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Seek(1, io.SeekCurrent); err == nil {
		t.Fatal("seek overflow accepted")
	}
	if _, err := reader.ReadAt(make([]byte, 1), -1); err == nil {
		t.Fatal("negative ReadAt accepted")
	}
	if _, err := f.Append(make([]byte, MaxRecordSize+1)); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversized record = %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, chunks[0]) || len(raw) <= len(want) {
		t.Fatal("physical file exposed plaintext or lacked record framing")
	}
	if _, err := Create(path, key, testIdentity); !errors.Is(err, os.ErrExist) {
		t.Fatalf("exclusive create = %v", err)
	}
	wrongKey, _ := GenerateKey()
	if _, err := Open(path, wrongKey, testIdentity); !errors.Is(err, ErrAuthentication) {
		t.Fatalf("wrong file key = %v", err)
	}
	if _, err := Open(path, key, "sessions/session-a/other-shell/events"); !errors.Is(err, ErrAuthentication) {
		t.Fatalf("cross-file replacement = %v", err)
	}
}

func TestFileWriterOwnershipAndReopenAppend(t *testing.T) {
	f, path, key := testFile(t)
	if _, err := f.Append([]byte("before")); err != nil {
		t.Fatal(err)
	}
	if writer, err := OpenAppend(path, key, testIdentity); !errors.Is(err, ErrBusy) {
		if writer != nil {
			_ = writer.Close()
		}
		t.Fatalf("competing writer = %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := f.ReadAt(make([]byte, 1), 0); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("closed ReadAt = %v", err)
	}
	f, err := OpenAppend(path, key, testIdentity)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if offset, err := f.Append([]byte("after")); err != nil || offset != 6 {
		t.Fatalf("resumed append offset=%d err=%v", offset, err)
	}
	data, err := io.ReadAll(f)
	if err != nil || string(data) != "beforeafter" {
		t.Fatal("reopened append changed the existing byte coordinate system")
	}
}

func TestFileRejectsCorruptionReorderingAndDeletion(t *testing.T) {
	f, path, key := testFile(t)
	for _, chunk := range []string{"alpha", "bravo", "charl"} {
		if _, err := f.Append([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	frame := recordOverhead + 5
	for _, tc := range []struct {
		name string
		edit func([]byte) []byte
		want error
	}{
		{"tag", func(b []byte) []byte { b[len(b)-1] ^= 1; return b }, ErrAuthentication},
		{"middle ciphertext", func(b []byte) []byte { b[fileHeaderSize+frame+recordPrefixSize] ^= 1; return b }, ErrAuthentication},
		{"header", func(b []byte) []byte { b[10] ^= 1; return b }, ErrAuthentication},
		{"version", func(b []byte) []byte { b[6]++; return b }, ErrVersion},
		{"unbounded length", func(b []byte) []byte { binary.LittleEndian.PutUint32(b[fileHeaderSize:], ^uint32(0)); return b }, ErrFormat},
		{"reordered records", func(b []byte) []byte {
			first := bytes.Clone(b[fileHeaderSize : fileHeaderSize+frame])
			copy(b[fileHeaderSize:], b[fileHeaderSize+frame:fileHeaderSize+2*frame])
			copy(b[fileHeaderSize+frame:], first)
			return b
		}, ErrAuthentication},
		{"deleted middle", func(b []byte) []byte { return append(b[:fileHeaderSize+frame], b[fileHeaderSize+2*frame:]...) }, ErrAuthentication},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := tc.edit(bytes.Clone(original))
			if err := os.WriteFile(path, changed, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := Open(path, key, testIdentity); !errors.Is(err, tc.want) {
				t.Fatalf("open corruption = %v, want %v", err, tc.want)
			}
			if _, err := RecoverTail(path, key, testIdentity); !errors.Is(err, tc.want) {
				t.Fatalf("recovery must refuse corruption: %v", err)
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(after, changed) {
				t.Fatal("recovery modified a corrupt complete record")
			}
		})
	}
}

func TestFileEveryTruncatedRecordBoundary(t *testing.T) {
	f, source, key := testFile(t)
	chunks := [][]byte{[]byte("first"), []byte("second\r\n")}
	for _, data := range chunks {
		if _, err := f.Append(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for cut := fileHeaderSize; cut < len(original); cut++ {
		path := filepath.Join(dir, strconv.Itoa(cut)+".enc")
		if err := os.WriteFile(path, original[:cut], 0600); err != nil {
			t.Fatal(err)
		}
		validPhysical, validLogical := fileHeaderSize, 0
		for _, chunk := range chunks {
			if validPhysical+recordOverhead+len(chunk) > cut {
				break
			}
			validPhysical += recordOverhead + len(chunk)
			validLogical += len(chunk)
		}
		info, err := Inspect(path, key, testIdentity)
		incomplete := cut != validPhysical
		if info.ValidPhysical != int64(validPhysical) || info.LogicalSize != int64(validLogical) || incomplete != errors.Is(err, ErrIncompleteTail) || (!incomplete && err != nil) {
			t.Fatalf("cut=%d prefix=%+v err=%v", cut, info, err)
		}
		info, err = RecoverTail(path, key, testIdentity)
		if err != nil {
			t.Fatalf("cut=%d recovery: %v", cut, err)
		}
		if incomplete {
			backup, err := os.ReadFile(info.BackupPath)
			if err != nil || !bytes.Equal(backup, original[:cut]) {
				t.Fatalf("cut=%d did not preserve the original ciphertext", cut)
			}
		} else if info.BackupPath != "" {
			t.Fatalf("cut=%d created a recovery backup for a complete file", cut)
		}
		resumed, err := OpenAppend(path, key, testIdentity)
		if err != nil {
			t.Fatal(err)
		}
		offset, appendErr := resumed.Append([]byte("resumed"))
		closeErr := resumed.Close()
		if appendErr != nil || closeErr != nil || offset != int64(validLogical) {
			t.Fatalf("cut=%d resumed offset=%d append=%v close=%v", cut, offset, appendErr, closeErr)
		}
		data, err := ReadFile(path, key, testIdentity)
		want := append(bytes.Clone(bytes.Join(chunks, nil)[:validLogical]), []byte("resumed")...)
		if err != nil || !bytes.Equal(data, want) {
			t.Fatalf("cut=%d resumed history changed", cut)
		}
	}
}

type partialRecordWriter struct{ f *os.File }

func (w partialRecordWriter) WriteAt(data []byte, offset int64) (int, error) {
	n, err := w.f.WriteAt(data[:len(data)/2], offset)
	if err != nil {
		return n, err
	}
	return n, io.ErrShortWrite
}

type failingSync struct{ err error }

func (s failingSync) Sync() error { return s.err }

func TestSyncFailureIsReportedAndCloseStillReleasesOwnership(t *testing.T) {
	f, path, key := testFile(t)
	if _, err := f.Append([]byte("readable but unacknowledged")); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("injected synchronization failure")
	f.syncer = failingSync{failure}
	if err := f.Sync(); !errors.Is(err, failure) {
		t.Fatalf("failed Sync returned a persistence acknowledgement: %v", err)
	}
	if err := f.Close(); !errors.Is(err, failure) {
		t.Fatalf("Close hid the synchronization failure: %v", err)
	}
	resumed, err := OpenAppend(path, key, testIdentity)
	if err != nil {
		t.Fatalf("failed Close retained writer ownership: %v", err)
	}
	defer resumed.Close()
	data, err := io.ReadAll(resumed)
	if err != nil || string(data) != "readable but unacknowledged" {
		t.Fatal("sync failure discarded a complete record")
	}
}

func TestPartialWriteIsNotPublishedAndPoisonsWriter(t *testing.T) {
	f, path, key := testFile(t)
	if _, err := f.Append([]byte("committed prefix")); err != nil {
		t.Fatal(err)
	}
	f.writer = partialRecordWriter{f.f}
	if _, err := f.Append([]byte("not committed")); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("injected partial write = %v", err)
	}
	if _, err := f.Append([]byte("must not follow a torn tail")); !errors.Is(err, ErrWriteFailed) {
		t.Fatalf("writer continued after partial write: %v", err)
	}
	data, err := io.ReadAll(f)
	if err != nil || string(data) != "committed prefix" {
		t.Fatal("partial write was exposed to live readers")
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenAppend(path, key, testIdentity); !errors.Is(err, ErrIncompleteTail) {
		t.Fatalf("reopen must require explicit recovery: %v", err)
	}
}

func TestFileConcurrentReadersAndAppenders(t *testing.T) {
	f, _, _ := testFile(t)
	type appended struct {
		offset int64
		data   []byte
	}
	results := make(chan appended, 128)
	errs := make(chan error, 5)
	done, readerDone := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(readerDone)
		for {
			select {
			case <-done:
				return
			default:
			}
			size, err := f.Size()
			if err != nil {
				errs <- err
				return
			}
			buf := make([]byte, min(size, 64))
			if _, err := f.ReadAt(buf, size-int64(len(buf))); err != nil {
				errs <- err
				return
			}
			runtime.Gosched()
		}
	}()
	var workers sync.WaitGroup
	for worker := 0; worker < 4; worker++ {
		workers.Add(1)
		go func(worker int) {
			defer workers.Done()
			for i := 0; i < 32; i++ {
				data := []byte(fmt.Sprintf("worker=%d record=%02d\n", worker, i))
				offset, err := f.Append(data)
				if err != nil {
					errs <- err
					return
				}
				results <- appended{offset, data}
			}
		}(worker)
	}
	workers.Wait()
	close(done)
	<-readerDone
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	close(results)
	var writes []appended
	for result := range results {
		writes = append(writes, result)
	}
	sort.Slice(writes, func(i, j int) bool { return writes[i].offset < writes[j].offset })
	var want []byte
	for _, write := range writes {
		if write.offset != int64(len(want)) {
			t.Fatal("concurrent appends produced overlapping or missing logical offsets")
		}
		want = append(want, write.data...)
	}
	got, err := io.ReadAll(f)
	if err != nil || !bytes.Equal(got, want) || len(writes) != 128 {
		t.Fatal("concurrent record history differs from the acknowledged writes")
	}
}

func TestAtomicReplacementAndFailedReplacementPreserveData(t *testing.T) {
	key, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "hosts.enc")
	want := bytes.Repeat([]byte("placeholder config content\n"), MaxRecordSize/16)
	if err := WriteFileAtomic(path, key, "config/hosts", want); err != nil {
		t.Fatal(err)
	}
	got, err := ReadFile(path, key, "config/hosts")
	if err != nil || !bytes.Equal(got, want) {
		t.Fatal("atomic multi-record file did not round trip")
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomic(path, key, "", []byte("refused update")); !errors.Is(err, ErrFormat) {
		t.Fatalf("invalid identity update = %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(after, before) {
		t.Fatal("failed replacement changed the previous file")
	}
	if err := WriteFileAtomic(path, key, "config/hosts", nil); err != nil {
		t.Fatal(err)
	}
	got, err = ReadFile(path, key, "config/hosts")
	if err != nil || len(got) != 0 {
		t.Fatal("atomic replacement with an empty file failed")
	}
	leftovers, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".tmp-enc-*"))
	if err != nil || len(leftovers) != 0 {
		t.Fatal("atomic replacement left temporary ciphertext files behind")
	}
}
