package sessionlog

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/open-mcp-ai/termcp/internal/encryptedfs"
)

const (
	indexHeaderSize = 104
	indexEntrySize  = 40
	maxIndexSize    = 64 << 20
)

var ErrIndex = errors.New("sessionlog: index does not match authenticated history")

// SaveIndex writes a rebuildable, encrypted binary checkpoint table. The log
// reaches Sync before the table is published, so a new index cannot deliberately
// point beyond a durable log prefix. An index error never removes history.
func (l *Log) SaveIndex(path string, indexKey encryptedfs.Key) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.checkIndexPath(path); err != nil {
		return err
	}
	if l.writable {
		if err := l.file.Sync(); err != nil {
			return err
		}
	}
	data, err := l.indexBytes()
	if err != nil {
		return err
	}
	defer clear(data)
	return encryptedfs.WriteFileAtomic(path, indexKey, l.identity+"/index", data)
}

// ValidateIndex never imports offsets from a sidecar. The authenticated log is
// scanned at Open and its reconstructed table is authoritative. A lost, stale,
// corrupt or foreign sidecar can be replaced without changing history.
// This first implementation still scans the full log at startup; the sparse
// table bounds byte/time lookup work after that scan.
func (l *Log) ValidateIndex(path string, indexKey encryptedfs.Key) error {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if err := l.checkIndexPath(path); err != nil {
		return err
	}
	expected, err := l.indexBytes()
	if err != nil {
		return err
	}
	defer clear(expected)
	actual, err := encryptedfs.ReadFile(path, indexKey, l.identity+"/index")
	if err != nil {
		return fmt.Errorf("%w: %w", ErrIndex, err)
	}
	defer clear(actual)
	if !bytes.Equal(actual, expected) {
		return ErrIndex
	}
	return nil
}

func (l *Log) checkIndexPath(path string) error {
	source, err := filepath.Abs(l.path)
	if err != nil {
		return err
	}
	target, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if source == target {
		return ErrIndex
	}
	targetInfo, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	sourceInfo, err := os.Stat(l.path)
	if err != nil {
		return err
	}
	if os.SameFile(sourceInfo, targetInfo) {
		return ErrIndex
	}
	return nil
}

func (l *Log) indexBytes() ([]byte, error) {
	snapshot, err := l.file.Snapshot()
	if err != nil {
		return nil, err
	}
	if len(l.checkpoints) > (maxIndexSize-indexHeaderSize)/indexEntrySize {
		return nil, encryptedfs.ErrTooLarge
	}
	b := make([]byte, indexHeaderSize+len(l.checkpoints)*indexEntrySize)
	copy(b[:8], "TCPSIDX\x00")
	binary.LittleEndian.PutUint16(b[8:10], 1)
	binary.LittleEndian.PutUint32(b[12:16], indexStride)
	copy(b[16:48], snapshot.FileID[:])
	for i, value := range []uint64{snapshot.Records, uint64(l.outputSize), uint64(snapshot.PlainSize), uint64(snapshot.PhysicalSize)} {
		binary.LittleEndian.PutUint64(b[48+i*8:], value)
	}
	copy(b[80:96], snapshot.LastTag[:])
	binary.LittleEndian.PutUint64(b[96:104], uint64(len(l.checkpoints)))
	for i, cp := range l.checkpoints {
		for j, value := range []uint64{cp.sequence, uint64(cp.encoded), uint64(cp.physical), uint64(cp.output), uint64(cp.maxTime)} {
			binary.LittleEndian.PutUint64(b[indexHeaderSize+i*indexEntrySize+j*8:], value)
		}
	}
	return b, nil
}
