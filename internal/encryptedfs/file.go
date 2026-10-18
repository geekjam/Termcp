package encryptedfs

import (
	"bytes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"sync"

	"golang.org/x/crypto/chacha20poly1305"
)

const (
	fileHeaderSize   = 80 // magic/version + file ID + nonce + authenticated empty payload
	recordPrefixSize = 4 + chacha20poly1305.NonceSizeX
	recordPlainSize  = 20 // sequence + logical offset + data length, all encrypted
	recordOverhead   = recordPrefixSize + recordPlainSize + chacha20poly1305.Overhead
	MaxRecordSize    = 1 << 20
)

var (
	ErrIncompleteTail = errors.New("encryptedfs: incomplete final record")
	ErrReadOnly       = errors.New("encryptedfs: file is read-only")
	ErrBusy           = errors.New("encryptedfs: another writer holds the file")
	ErrWriteFailed    = errors.New("encryptedfs: append stopped after a write failure")
	ErrTooLarge       = errors.New("encryptedfs: record exceeds size limit")
)

type recordRef struct {
	physical int64
	logical  int64
	length   uint32
	dataSize uint32
	previous [chacha20poly1305.Overhead]byte
}

// RecoveryInfo describes an authenticated prefix. A non-nil error must never be
// ignored: complete but unauthentic records are not eligible for tail repair.
type RecoveryInfo struct {
	PhysicalSize  int64
	ValidPhysical int64
	LogicalSize   int64
	Records       int64
	BackupPath    string
}

// Record is an authenticated append boundary. Offset belongs to the generic
// plaintext file, not a session's output projection. Data is owned by the caller.
type Record struct {
	Sequence       uint64
	Offset         int64
	PhysicalOffset int64
	Data           []byte
}

type Snapshot struct {
	FileID       [32]byte
	Records      uint64
	PlainSize    int64
	PhysicalSize int64
	LastTag      [16]byte
}

// File exposes plaintext bytes backed by independently authenticated records.
// One append is one record, limited to MaxRecordSize. ReadAt and Append may run
// concurrently; only complete writes become visible through this handle.
// Its in-memory record table can later be replaced by a sparse persistent index.
type File struct {
	mu       sync.RWMutex
	position sync.Mutex
	f        *os.File
	writer   io.WriterAt
	syncer   interface{ Sync() error }
	aead     cipher.AEAD
	header   [fileHeaderSize]byte
	refs     []recordRef
	previous [chacha20poly1305.Overhead]byte
	size     int64
	physical int64
	cursor   int64
	writable bool
	closed   bool
	failed   bool
}

var _ interface {
	io.Reader
	io.ReaderAt
	io.Seeker
	io.Closer
} = (*File)(nil)

func fileCipher(key Key, header []byte, identity string) (cipher.AEAD, error) {
	derived, err := deriveKey(key, header[8:40], "file", identity)
	if err != nil {
		return nil, err
	}
	aead, err := chacha20poly1305.NewX(derived[:])
	clear(derived[:])
	return aead, err
}

// Create exclusively creates a new file; it never overwrites an existing file.
func Create(path string, key Key, identity string) (*File, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, err
	}
	file, err := createHandle(f, key, identity)
	if err == nil {
		err = syncParent(path)
	}
	if err != nil {
		if file != nil {
			_ = unlockWriter(f)
		}
		_ = f.Close()
		_ = os.Remove(path)
		return nil, err
	}
	return file, nil
}

func createHandle(f *os.File, key Key, identity string) (_ *File, err error) {
	if err := lockWriter(f); err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = unlockWriter(f)
		}
	}()
	file := &File{f: f, writer: f, syncer: f, writable: true, physical: fileHeaderSize}
	copy(file.header[:6], "TCPEFS")
	binary.LittleEndian.PutUint16(file.header[6:8], formatVersion)
	if _, err := rand.Read(file.header[8:64]); err != nil {
		return nil, err
	}
	aead, err := fileCipher(key, file.header[:], identity)
	if err != nil {
		return nil, err
	}
	file.aead = aead
	copy(file.header[64:], aead.Seal(nil, file.header[40:64], nil, file.header[:64]))
	copy(file.previous[:], file.header[64:])
	if err := writeAllAt(f, file.header[:], 0); err != nil {
		return nil, err
	}
	if err := f.Sync(); err != nil {
		return nil, err
	}
	return file, nil
}

func Open(path string, key Key, identity string) (*File, error) {
	return openFile(path, key, identity, false)
}

// OpenAppend validates the entire existing file before allowing new appends.
// It does not repair incomplete tails; repair must be an explicit operation.
func OpenAppend(path string, key Key, identity string) (*File, error) {
	return openFile(path, key, identity, true)
}

func openFile(path string, key Key, identity string, writable bool) (*File, error) {
	flag := os.O_RDONLY
	if writable {
		flag = os.O_RDWR
	}
	f, err := os.OpenFile(path, flag, 0)
	if err != nil {
		return nil, err
	}
	if writable {
		if err := lockWriter(f); err != nil {
			_ = f.Close()
			return nil, err
		}
	}
	file, err := loadHeader(f, key, identity)
	if err == nil {
		_, err = file.scan()
	}
	if err != nil {
		if writable {
			_ = unlockWriter(f)
		}
		_ = f.Close()
		return nil, err
	}
	file.writable = writable
	return file, nil
}

func loadHeader(f *os.File, key Key, identity string) (*File, error) {
	file := &File{f: f, writer: f, syncer: f, physical: fileHeaderSize}
	if _, err := f.ReadAt(file.header[:], 0); err != nil {
		return nil, fmt.Errorf("%w: incomplete file header", ErrFormat)
	}
	if !bytes.Equal(file.header[:6], []byte("TCPEFS")) {
		return nil, ErrFormat
	}
	if binary.LittleEndian.Uint16(file.header[6:8]) != formatVersion {
		return nil, ErrVersion
	}
	aead, err := fileCipher(key, file.header[:], identity)
	if err != nil {
		return nil, err
	}
	if _, err := aead.Open(nil, file.header[40:64], file.header[64:], file.header[:64]); err != nil {
		return nil, ErrAuthentication
	}
	file.aead = aead
	copy(file.previous[:], file.header[64:])
	return file, nil
}

func (f *File) aad(prefix []byte, previous [chacha20poly1305.Overhead]byte) []byte {
	aad := make([]byte, 0, fileHeaderSize+len(previous)+len(prefix))
	aad = append(aad, f.header[:]...)
	aad = append(aad, previous[:]...)
	return append(aad, prefix...)
}

// decode validates framing before allocating and authenticates all plaintext
// before returning it. limit is the physical snapshot this read may observe.
func (f *File) decode(ref recordRef, sequence uint64, limit int64) ([]byte, uint32, [16]byte, error) {
	var tag [16]byte
	if limit-ref.physical < 4 {
		return nil, 0, tag, ErrIncompleteTail
	}
	var prefix [recordPrefixSize]byte
	if _, err := f.f.ReadAt(prefix[:4], ref.physical); err != nil {
		return nil, 0, tag, err
	}
	length := binary.LittleEndian.Uint32(prefix[:4])
	if length <= recordOverhead || length > MaxRecordSize+recordOverhead {
		return nil, 0, tag, ErrFormat
	}
	if int64(length) > limit-ref.physical {
		return nil, length, tag, ErrIncompleteTail
	}
	if _, err := f.f.ReadAt(prefix[4:], ref.physical+4); err != nil {
		return nil, 0, tag, err
	}
	ciphertext := make([]byte, int(length)-recordPrefixSize)
	if _, err := f.f.ReadAt(ciphertext, ref.physical+recordPrefixSize); err != nil {
		return nil, 0, tag, err
	}
	plaintext, err := f.aead.Open(nil, prefix[4:], ciphertext, f.aad(prefix[:], ref.previous))
	if err != nil {
		return nil, 0, tag, ErrAuthentication
	}
	if len(plaintext) < recordPlainSize || binary.LittleEndian.Uint64(plaintext[:8]) != sequence || binary.LittleEndian.Uint64(plaintext[8:16]) != uint64(ref.logical) || binary.LittleEndian.Uint32(plaintext[16:20]) != uint32(len(plaintext)-recordPlainSize) {
		clear(plaintext)
		return nil, 0, tag, ErrFormat
	}
	copy(tag[:], ciphertext[len(ciphertext)-len(tag):])
	return plaintext[recordPlainSize:], length, tag, nil
}

func (f *File) scan() (RecoveryInfo, error) {
	stat, err := f.f.Stat()
	if err != nil {
		return RecoveryInfo{}, err
	}
	info := RecoveryInfo{PhysicalSize: stat.Size(), ValidPhysical: fileHeaderSize}
	for f.physical < stat.Size() {
		ref := recordRef{physical: f.physical, logical: f.size, previous: f.previous}
		data, length, tag, err := f.decode(ref, uint64(len(f.refs)), stat.Size())
		if err != nil {
			return info, err
		}
		if int64(len(data)) > math.MaxInt64-f.size {
			clear(data)
			return info, ErrTooLarge
		}
		ref.length, ref.dataSize = length, uint32(len(data))
		f.refs = append(f.refs, ref)
		f.size += int64(len(data))
		f.physical += int64(length)
		f.previous = tag
		clear(data)
		info.ValidPhysical, info.LogicalSize, info.Records = f.physical, f.size, int64(len(f.refs))
	}
	return info, nil
}

// Append returns the logical starting offset. A write error poisons this writer;
// callers must close and inspect/recover it before writing again. Success means
// readable, not power-loss durable: call Sync for a persistence acknowledgement.
func (f *File) Append(data []byte) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	start := f.size
	if f.closed {
		return start, os.ErrClosed
	}
	if !f.writable {
		return start, ErrReadOnly
	}
	if f.failed {
		return start, ErrWriteFailed
	}
	if len(data) > MaxRecordSize || int64(len(data)) > math.MaxInt64-f.size || int64(len(data)+recordOverhead) > math.MaxInt64-f.physical {
		return start, ErrTooLarge
	}
	if len(data) == 0 {
		return start, nil
	}
	frame := make([]byte, recordPrefixSize, len(data)+recordOverhead)
	binary.LittleEndian.PutUint32(frame[:4], uint32(len(data)+recordOverhead))
	if _, err := rand.Read(frame[4:]); err != nil {
		return start, err
	}
	plain := make([]byte, recordPlainSize+len(data))
	binary.LittleEndian.PutUint64(plain[:8], uint64(len(f.refs)))
	binary.LittleEndian.PutUint64(plain[8:16], uint64(start))
	binary.LittleEndian.PutUint32(plain[16:20], uint32(len(data)))
	copy(plain[recordPlainSize:], data)
	frame = f.aead.Seal(frame, frame[4:recordPrefixSize], plain, f.aad(frame, f.previous))
	clear(plain)
	if err := writeAllAt(f.writer, frame, f.physical); err != nil {
		f.failed = true
		return start, err
	}
	f.refs = append(f.refs, recordRef{physical: f.physical, logical: start, previous: f.previous, length: uint32(len(frame)), dataSize: uint32(len(data))})
	f.size += int64(len(data))
	f.physical += int64(len(frame))
	copy(f.previous[:], frame[len(frame)-len(f.previous):])
	return start, nil
}

func writeAllAt(f io.WriterAt, data []byte, offset int64) error {
	n, err := f.WriteAt(data, offset)
	if err == nil && n != len(data) {
		return io.ErrShortWrite
	}
	return err
}

func (f *File) Size() (int64, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if f.closed {
		return 0, os.ErrClosed
	}
	return f.size, nil
}

// Snapshot identifies the complete prefix published by this handle. A read-only
// handle is a snapshot taken at Open; live readers should share the append handle.
func (f *File) Snapshot() (Snapshot, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if f.closed {
		return Snapshot{}, os.ErrClosed
	}
	var id [32]byte
	copy(id[:], f.header[8:40])
	return Snapshot{id, uint64(len(f.refs)), f.size, f.physical, f.previous}, nil
}

// ReadRecord exposes authenticated record boundaries to business codecs and
// index builders. It does not let an index supply unauthenticated seek state.
func (f *File) ReadRecord(sequence uint64) (Record, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if f.closed {
		return Record{}, os.ErrClosed
	}
	if sequence >= uint64(len(f.refs)) {
		return Record{}, io.EOF
	}
	ref := f.refs[sequence]
	data, length, _, err := f.decode(ref, sequence, f.physical)
	if err != nil {
		return Record{}, err
	}
	if length != ref.length || len(data) != int(ref.dataSize) {
		clear(data)
		return Record{}, ErrFormat
	}
	return Record{sequence, ref.logical, ref.physical, data}, nil
}

func (f *File) ReadAt(buf []byte, offset int64) (int, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if f.closed {
		return 0, os.ErrClosed
	}
	if offset < 0 {
		return 0, fmt.Errorf("encryptedfs: negative read offset")
	}
	if len(buf) == 0 {
		return 0, nil
	}
	if offset >= f.size {
		return 0, io.EOF
	}
	n := 0
	index := sort.Search(len(f.refs), func(i int) bool { return f.refs[i].logical+int64(f.refs[i].dataSize) > offset })
	for index < len(f.refs) && n < len(buf) {
		ref := f.refs[index]
		data, length, _, err := f.decode(ref, uint64(index), f.physical)
		if err != nil {
			return n, err
		}
		if length != ref.length || len(data) != int(ref.dataSize) {
			clear(data)
			return n, ErrFormat
		}
		copied := copy(buf[n:], data[offset-ref.logical:])
		clear(data)
		n += copied
		offset += int64(copied)
		index++
	}
	if n < len(buf) {
		return n, io.EOF
	}
	return n, nil
}

func (f *File) Read(buf []byte) (int, error) {
	f.position.Lock()
	defer f.position.Unlock()
	n, err := f.ReadAt(buf, f.cursor)
	f.cursor += int64(n)
	return n, err
}

func (f *File) Seek(offset int64, whence int) (int64, error) {
	f.position.Lock()
	defer f.position.Unlock()
	size, err := f.Size()
	if err != nil {
		return 0, err
	}
	var base int64
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		base = f.cursor
	case io.SeekEnd:
		base = size
	default:
		return 0, fmt.Errorf("encryptedfs: invalid seek origin")
	}
	if (offset > 0 && base > math.MaxInt64-offset) || offset < -base {
		return 0, fmt.Errorf("encryptedfs: invalid seek offset")
	}
	f.cursor = base + offset
	return f.cursor, nil
}

func (f *File) Sync() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return os.ErrClosed
	}
	if !f.writable {
		return ErrReadOnly
	}
	return f.syncer.Sync()
}

func (f *File) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil
	}
	f.closed = true
	var syncErr error
	var unlockErr error
	if f.writable {
		syncErr = f.syncer.Sync()
		unlockErr = unlockWriter(f.f)
	}
	return errors.Join(syncErr, unlockErr, f.f.Close())
}

// Inspect authenticates a physical snapshot without changing the file.
func Inspect(path string, key Key, identity string) (RecoveryInfo, error) {
	f, err := os.Open(path)
	if err != nil {
		return RecoveryInfo{}, err
	}
	defer f.Close()
	file, err := loadHeader(f, key, identity)
	if err != nil {
		return RecoveryInfo{}, err
	}
	return file.scan()
}

// RecoverTail explicitly repairs only incomplete final framing. The complete
// original ciphertext is copied and synced before truncation. Authentication or
// framing failures are returned unchanged and never cause automatic truncation.
// Incomplete framing alone cannot prove a crash: callers must choose the repair
// policy explicitly, since malicious length changes can also look incomplete.
func RecoverTail(path string, key Key, identity string) (info RecoveryInfo, err error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return info, err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	if err := lockWriter(f); err != nil {
		return info, err
	}
	defer func() { err = errors.Join(err, unlockWriter(f)) }()
	file, err := loadHeader(f, key, identity)
	if err != nil {
		return info, err
	}
	info, err = file.scan()
	if !errors.Is(err, ErrIncompleteTail) {
		return info, err
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return info, err
	}
	backupPath := fmt.Sprintf("%s.recovery-%x", path, id)
	backup, err := os.OpenFile(backupPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return info, err
	}
	copied, copyErr := io.Copy(backup, io.NewSectionReader(f, 0, info.PhysicalSize))
	if copyErr == nil && copied != info.PhysicalSize {
		copyErr = io.ErrUnexpectedEOF
	}
	backupErr := errors.Join(copyErr, backup.Sync(), backup.Close())
	if backupErr != nil {
		_ = os.Remove(backupPath)
		return info, backupErr
	}
	info.BackupPath = backupPath
	if err := syncParent(path); err != nil {
		return info, err
	}
	if err := f.Truncate(info.ValidPhysical); err != nil {
		return info, err
	}
	return info, f.Sync()
}
