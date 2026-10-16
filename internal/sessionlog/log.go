// Package sessionlog records shell events on top of encryptedfs. Its byte view
// contains output only: event headers, resize events and marks never advance the
// public output offset. No raw stdin recording is provided.
package sessionlog

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"sync"
	"unicode/utf8"

	"github.com/open-mcp-ai/termcp/internal/encryptedfs"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

const (
	eventVersion = 1
	eventHeader  = 24
	indexStride  = 64
	MaxOutput    = encryptedfs.MaxRecordSize - eventHeader
	MaxTimeRead  = 4 << 20
)

var ErrEvent = errors.New("sessionlog: invalid event")

type Type uint8

const (
	Output Type = 1 + iota
	Mark
	Resize
)

type Stream uint8

const (
	Stdout Stream = iota
	Stderr
)

type Event struct {
	Sequence     uint64
	TimestampMS  int64
	OutputOffset int64
	Type         Type
	Stream       Stream
	Data         []byte
	Mark         *api.LogMark
	Cols         uint16
	Rows         uint16
}

type checkpoint struct {
	sequence uint64
	encoded  int64
	physical int64
	output   int64
	maxTime  int64 // maximum output timestamp through this block; -1 if none
}

// Log owns the underlying encrypted file. Live readers share this object;
// separately opened readers observe the prefix present when they were opened.
type Log struct {
	mu          sync.RWMutex
	file        *encryptedfs.File
	path        string
	identity    string
	writable    bool
	records     uint64
	outputSize  int64
	checkpoints []checkpoint
	lines       []int64
	streams     []int64
	marks       []api.LogMark
}

func Create(path string, key encryptedfs.Key, identity string) (*Log, error) {
	f, err := encryptedfs.Create(path, key, identity)
	if err != nil {
		return nil, err
	}
	return &Log{file: f, path: path, identity: identity, writable: true}, nil
}

func Open(path string, key encryptedfs.Key, identity string) (*Log, error) {
	return open(path, key, identity, false)
}

// OpenAppend refuses incomplete tails. An application must explicitly inspect
// and recover the encrypted file before resuming, rather than hiding corruption.
func OpenAppend(path string, key encryptedfs.Key, identity string) (*Log, error) {
	return open(path, key, identity, true)
}

func open(path string, key encryptedfs.Key, identity string, writable bool) (*Log, error) {
	openFile := encryptedfs.Open
	if writable {
		openFile = encryptedfs.OpenAppend
	}
	f, err := openFile(path, key, identity)
	if err != nil {
		return nil, err
	}
	l := &Log{file: f, path: path, identity: identity, writable: writable}
	snapshot, err := f.Snapshot()
	if err == nil {
		for sequence := uint64(0); sequence < snapshot.Records; sequence++ {
			var record encryptedfs.Record
			record, err = f.ReadRecord(sequence)
			if err != nil {
				break
			}
			var event Event
			event, err = decode(record)
			clear(record.Data)
			if err == nil {
				err = l.checkPosition(event)
			}
			if err == nil {
				l.publish(event, record.Offset, record.PhysicalOffset)
			}
			clear(event.Data)
			if err != nil {
				break
			}
		}
	}
	if err != nil {
		return nil, errors.Join(err, f.Close())
	}
	return l, nil
}

func (l *Log) checkPosition(e Event) error {
	if e.Sequence != l.records || e.OutputOffset != l.outputSize || int64(len(e.Data)) > math.MaxInt64-l.outputSize {
		return ErrEvent
	}
	return nil
}

func (l *Log) publish(e Event, encoded, physical int64) {
	if l.records%indexStride == 0 {
		maxTime := int64(-1)
		if len(l.checkpoints) != 0 {
			maxTime = l.checkpoints[len(l.checkpoints)-1].maxTime
		}
		l.checkpoints = append(l.checkpoints, checkpoint{l.records, encoded, physical, l.outputSize, maxTime})
	}
	if e.Type == Output {
		cp := &l.checkpoints[len(l.checkpoints)-1]
		cp.maxTime = max(cp.maxTime, e.TimestampMS)
		l.outputSize += int64(len(e.Data))
		for i, b := range e.Data {
			if b == '\n' {
				l.lines = append(l.lines, e.OutputOffset+int64(i))
			}
		}
		streamRecord := e.OutputOffset + 1
		if e.Stream == Stderr {
			streamRecord = -streamRecord
		}
		if len(l.streams) == 0 || (l.streams[len(l.streams)-1] < 0) != (streamRecord < 0) {
			l.streams = append(l.streams, streamRecord)
		}
	} else if e.Type == Mark {
		l.marks = append(l.marks, *e.Mark)
	}
	l.records++
}

// RecordOutput appends one bounded output chunk and returns its public byte
// offset. The caller splits larger chunks before recording them.
func (l *Log) RecordOutput(timestampMS int64, data []byte, stream Stream) (int64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	offset := l.outputSize
	if len(data) == 0 {
		// Ask the file too, so an empty write cannot silently succeed after Close.
		_, err := l.file.Size()
		if err == nil && !l.writable {
			err = encryptedfs.ErrReadOnly
		}
		return offset, err
	}
	err := l.append(Event{TimestampMS: timestampMS, OutputOffset: offset, Type: Output, Stream: stream, Data: data})
	return offset, err
}

func (l *Log) RecordMark(mark api.LogMark) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.append(Event{TimestampMS: mark.Time, OutputOffset: l.outputSize, Type: Mark, Mark: &mark})
}

func (l *Log) RecordResize(timestampMS int64, cols, rows uint16) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.append(Event{TimestampMS: timestampMS, OutputOffset: l.outputSize, Type: Resize, Cols: cols, Rows: rows})
}

func (l *Log) append(e Event) error {
	e.Sequence = l.records
	if err := l.checkPosition(e); err != nil {
		return err
	}
	encoded, err := encode(e)
	if err != nil {
		return err
	}
	defer clear(encoded)
	snapshot, err := l.file.Snapshot()
	if err != nil {
		return err
	}
	if _, err := l.file.Append(encoded); err != nil {
		return err
	}
	l.publish(e, snapshot.PlainSize, snapshot.PhysicalSize)
	return nil
}

func (l *Log) Size() (int64, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if _, err := l.file.Size(); err != nil {
		return 0, err
	}
	return l.outputSize, nil
}

func (l *Log) ReadEvent(sequence uint64) (Event, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.readEvent(sequence)
}

func (l *Log) readEvent(sequence uint64) (Event, error) {
	record, err := l.file.ReadRecord(sequence)
	if err != nil {
		return Event{}, err
	}
	defer clear(record.Data)
	return decode(record)
}

func (l *Log) ReadAt(buf []byte, offset int64) (int, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.readAt(buf, offset)
}

func (l *Log) readAt(buf []byte, offset int64) (int, error) {
	if _, err := l.file.Size(); err != nil {
		return 0, err
	}
	if offset < 0 {
		return 0, ErrEvent
	}
	if len(buf) == 0 {
		return 0, nil
	}
	if offset >= l.outputSize {
		return 0, io.EOF
	}
	i := sort.Search(len(l.checkpoints), func(i int) bool { return l.checkpoints[i].output > offset }) - 1
	cp := l.checkpoints[max(i, 0)]
	n, position := 0, cp.output
	for sequence := cp.sequence; sequence < l.records && n < len(buf); sequence++ {
		e, err := l.readEvent(sequence)
		if err != nil {
			return n, err
		}
		if e.OutputOffset != position {
			clear(e.Data)
			return n, ErrEvent
		}
		position += int64(len(e.Data))
		if e.Type == Output && offset < position {
			copied := copy(buf[n:], e.Data[offset-e.OutputOffset:])
			n += copied
			offset += int64(copied)
		}
		clear(e.Data)
	}
	if n < len(buf) {
		return n, io.EOF
	}
	return n, nil
}

// ReadByTime starts at the first output event in append order whose Unix
// millisecond timestamp is >= timestampMS. Prefix maxima permit backwards wall
// clock changes without treating the timestamps as a sorted sequence.
func (l *Log) ReadByTime(timestampMS int64, maxBytes int) ([]byte, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if _, err := l.file.Size(); err != nil {
		return nil, err
	}
	if timestampMS < 0 || maxBytes <= 0 || maxBytes > MaxTimeRead {
		return nil, ErrEvent
	}
	i := sort.Search(len(l.checkpoints), func(i int) bool { return l.checkpoints[i].maxTime >= timestampMS })
	if i < len(l.checkpoints) {
		cp := l.checkpoints[i]
		end := min(cp.sequence+indexStride, l.records)
		for sequence := cp.sequence; sequence < end; sequence++ {
			e, err := l.readEvent(sequence)
			if err != nil {
				return nil, err
			}
			found, offset := e.Type == Output && e.TimestampMS >= timestampMS, e.OutputOffset
			clear(e.Data)
			if found {
				buf := make([]byte, int(min(int64(maxBytes), l.outputSize-offset)))
				n, err := l.readAt(buf, offset)
				return buf[:n], err
			}
		}
		return nil, ErrEvent // authenticated history and its derived table disagree
	}
	return nil, io.EOF
}

func (l *Log) Sync() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.file.Sync()
}

func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.file.Close()
}

func validate(e Event) error {
	if e.TimestampMS < 0 || e.OutputOffset < 0 || e.Stream > Stderr {
		return ErrEvent
	}
	switch e.Type {
	case Output:
		if len(e.Data) == 0 || len(e.Data) > MaxOutput || e.Mark != nil || e.Cols != 0 || e.Rows != 0 {
			return ErrEvent
		}
	case Mark:
		if e.Stream != Stdout || e.Mark == nil || len(e.Mark.Status) == 0 || len(e.Mark.Status) > 1024 || !utf8.ValidString(string(e.Mark.Status)) || e.Mark.Offset < 0 || e.Mark.Offset > e.OutputOffset || e.Mark.Time != e.TimestampMS || len(e.Data) != 0 || e.Cols != 0 || e.Rows != 0 {
			return ErrEvent
		}
	case Resize:
		if e.Stream != Stdout || e.Cols == 0 || e.Rows == 0 || len(e.Data) != 0 || e.Mark != nil {
			return ErrEvent
		}
	default:
		return ErrEvent
	}
	return nil
}

func encode(e Event) ([]byte, error) {
	if err := validate(e); err != nil {
		return nil, err
	}
	var payload []byte
	switch e.Type {
	case Output:
		payload = e.Data
	case Mark:
		payload, _ = json.Marshal(e.Mark)
	case Resize:
		payload = make([]byte, 4)
		binary.LittleEndian.PutUint16(payload[:2], e.Cols)
		binary.LittleEndian.PutUint16(payload[2:], e.Rows)
	}
	encoded := make([]byte, eventHeader+len(payload))
	binary.LittleEndian.PutUint16(encoded[:2], eventVersion)
	encoded[2], encoded[3] = byte(e.Type), byte(e.Stream)
	binary.LittleEndian.PutUint64(encoded[4:12], uint64(e.TimestampMS))
	binary.LittleEndian.PutUint64(encoded[12:20], uint64(e.OutputOffset))
	binary.LittleEndian.PutUint32(encoded[20:24], uint32(len(payload)))
	copy(encoded[eventHeader:], payload)
	return encoded, nil
}

func decode(record encryptedfs.Record) (Event, error) {
	b := record.Data
	if len(b) < eventHeader || len(b) > encryptedfs.MaxRecordSize || binary.LittleEndian.Uint16(b[:2]) != eventVersion || binary.LittleEndian.Uint32(b[20:24]) != uint32(len(b)-eventHeader) {
		return Event{}, ErrEvent
	}
	e := Event{Sequence: record.Sequence, Type: Type(b[2]), Stream: Stream(b[3]), TimestampMS: int64(binary.LittleEndian.Uint64(b[4:12])), OutputOffset: int64(binary.LittleEndian.Uint64(b[12:20]))}
	payload := b[eventHeader:]
	switch e.Type {
	case Output:
		e.Data = append([]byte(nil), payload...)
	case Mark:
		e.Mark = new(api.LogMark)
		if len(payload) > 8192 || json.Unmarshal(payload, e.Mark) != nil {
			return Event{}, ErrEvent
		}
	case Resize:
		if len(payload) != 4 {
			return Event{}, ErrEvent
		}
		e.Cols, e.Rows = binary.LittleEndian.Uint16(payload[:2]), binary.LittleEndian.Uint16(payload[2:])
	default:
		return Event{}, ErrEvent
	}
	if err := validate(e); err != nil {
		clear(e.Data)
		return Event{}, fmt.Errorf("%w: sequence %d", err, record.Sequence)
	}
	return e, nil
}
