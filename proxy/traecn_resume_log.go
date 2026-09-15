package proxy

import (
	"errors"
	"io"
	"os"
	"sync/atomic"
)

var errTraeCNResumeStorage = errors.New("TRAE resume storage limit or write failure")
var traeCNResumeStoredBytes atomic.Int64

const (
	traeCNResumeMemoryBytes = 1 << 20
	traeCNResumeTaskBytes   = 64 << 20
	traeCNResumeGlobalBytes = 256 << 20
	traeCNResumeRecordLimit = 65536
)

type traeCNResumeRecord struct {
	offset int64
	size   int
	itemID string
}

// traeCNResumeLog 由任务锁保护，记录完整事件；超过内存阈值后释放内存并写入临时文件。
type traeCNResumeLog struct {
	memory         []byte
	file           *os.File
	size, reserved int64
	records        []traeCNResumeRecord
}

func (l *traeCNResumeLog) append(data []byte, itemID string) error {
	cost := int64(len(data) + 128 + len(itemID))
	if l.size+int64(len(data)) > traeCNResumeTaskBytes || len(l.records) >= traeCNResumeRecordLimit {
		return errTraeCNResumeStorage
	}
	if traeCNResumeStoredBytes.Add(cost) > traeCNResumeGlobalBytes {
		traeCNResumeStoredBytes.Add(-cost)
		return errTraeCNResumeStorage
	}
	ok := false
	defer func() {
		if !ok {
			traeCNResumeStoredBytes.Add(-cost)
		}
	}()
	if l.file == nil && l.size+int64(len(data)) > traeCNResumeMemoryBytes {
		file, err := os.CreateTemp("", "codex2api-traecn-resume-*")
		if err != nil {
			return errTraeCNResumeStorage
		}
		if _, err = file.Write(l.memory); err != nil {
			name := file.Name()
			_ = file.Close()
			_ = os.Remove(name)
			return errTraeCNResumeStorage
		}
		l.file, l.memory = file, nil
	}
	if l.file != nil {
		if n, err := l.file.Write(data); err != nil || n != len(data) {
			return errTraeCNResumeStorage
		}
	} else {
		l.memory = append(l.memory, data...)
	}
	l.records = append(l.records, traeCNResumeRecord{offset: l.size, size: len(data), itemID: itemID})
	l.size += int64(len(data))
	l.reserved += cost
	ok = true
	return nil
}

func (l *traeCNResumeLog) read(index int) ([]byte, error) {
	record := l.records[index]
	data := make([]byte, record.size)
	if l.file == nil {
		copy(data, l.memory[record.offset:record.offset+int64(record.size)])
		return data, nil
	}
	n, err := l.file.ReadAt(data, record.offset)
	if err != nil && err != io.EOF {
		return nil, err
	}
	if n != len(data) {
		return nil, io.ErrUnexpectedEOF
	}
	return data, nil
}

func (l *traeCNResumeLog) close() {
	if l.file != nil {
		name := l.file.Name()
		_ = l.file.Close()
		_ = os.Remove(name)
		l.file = nil
	}
	traeCNResumeStoredBytes.Add(-l.reserved)
	l.memory, l.records, l.size, l.reserved = nil, nil, 0, 0
}
