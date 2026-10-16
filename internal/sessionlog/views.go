package sessionlog

import (
	"github.com/open-mcp-ai/termcp/pkg/api"
)

// These views are rebuilt from authenticated events. They never read legacy
// plaintext sidecars or use an index to decide what history exists.
func (l *Log) LineRecordCount() (int64, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if _, err := l.file.Size(); err != nil {
		return 0, err
	}
	return int64(len(l.lines)), nil
}

func (l *Log) ReadLineRecords(first int64, count int) ([]int64, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if _, err := l.file.Size(); err != nil {
		return nil, err
	}
	if first < 0 || count <= 0 || first >= int64(len(l.lines)) {
		return nil, nil
	}
	n := min(int64(count), int64(len(l.lines))-first)
	return append([]int64(nil), l.lines[first:first+n]...), nil
}

func (l *Log) ReadStdioRecords() ([]int64, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if _, err := l.file.Size(); err != nil {
		return nil, err
	}
	return append([]int64(nil), l.streams...), nil
}

func (l *Log) ReadMarks() ([]api.LogMark, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if _, err := l.file.Size(); err != nil {
		return nil, err
	}
	return append([]api.LogMark(nil), l.marks...), nil
}

// ReadMarksWindow retains all statuses at the preceding offset (submitted input
// and its approval may share one) and reports the first following span boundary.
func (l *Log) ReadMarksWindow(start, end int64) ([]api.LogMark, int64, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if _, err := l.file.Size(); err != nil {
		return nil, 0, err
	}
	var below, out []api.LogMark
	var closer int64
	for _, mark := range l.marks {
		if mark.Offset <= start {
			if len(below) != 0 && below[len(below)-1].Offset != mark.Offset {
				below = below[:0]
			}
			below = append(below, mark)
			continue
		}
		if mark.Offset >= end {
			closer = mark.Offset
			break
		}
		out = append(out, mark)
	}
	return append(below, out...), closer, nil
}
