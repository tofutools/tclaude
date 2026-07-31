package filefollow

import (
	"bufio"
	"io"
	"os"
)

// Line is one complete newline-terminated record. Data is bounded to the
// configured maximum; Bytes always reports the full physical record length.
type Line struct {
	Data      []byte
	Bytes     int64
	Oversized bool
}

// TailInitialOffset returns a bounded-hydration offset selector suitable for
// Config.InitialOffset. It seeks at most maxBytes from EOF, then advances past
// the first newline so scanners never begin in the middle of a record. A
// single spanning record is drained in bounded bufio fragments.
func TailInitialOffset(maxBytes int64) func(*os.File, os.FileInfo) (int64, error) {
	return func(file *os.File, info os.FileInfo) (int64, error) {
		if maxBytes <= 0 || info.Size() <= maxBytes {
			return 0, nil
		}
		start := info.Size() - maxBytes
		if _, err := file.Seek(start, io.SeekStart); err != nil {
			return 0, err
		}
		reader := bufio.NewReaderSize(file, 64*1024)
		for {
			fragment, err := reader.ReadSlice('\n')
			start += int64(len(fragment))
			switch err {
			case nil:
				return start, nil
			case bufio.ErrBufferFull:
				continue
			case io.EOF:
				return info.Size(), nil
			default:
				return 0, err
			}
		}
	}
}

type LineConfig struct {
	MaxRecordBytes int
	BufferBytes    int
}

// ScanLines drains complete newline-terminated records without retaining more
// than MaxRecordBytes for any one record. An incomplete EOF tail is read but
// not committed, so the next append retries it from the same cursor.
func ScanLines(r io.Reader, config LineConfig, consume func(Line) bool, strict bool) (consumed int64, doubt bool, err error) {
	if config.MaxRecordBytes <= 0 {
		return 0, false, nil
	}
	if config.BufferBytes <= 0 {
		config.BufferBytes = 64 * 1024
	}
	reader := bufio.NewReaderSize(r, config.BufferBytes)
	line := make([]byte, 0, min(config.BufferBytes, config.MaxRecordBytes))
	for {
		line = line[:0]
		var lineBytes int64
		oversized := false
		for {
			fragment, readErr := reader.ReadSlice('\n')
			lineBytes += int64(len(fragment))
			if !oversized {
				remaining := config.MaxRecordBytes - len(line)
				if len(fragment) <= remaining {
					line = append(line, fragment...)
				} else {
					line = append(line, fragment[:remaining]...)
					oversized = true
				}
			}
			switch readErr {
			case bufio.ErrBufferFull:
				continue
			case nil:
				ok := consume(Line{Data: line, Bytes: lineBytes, Oversized: oversized})
				if strict && !ok {
					doubt = true
				}
				consumed += lineBytes
			case io.EOF:
				return consumed, doubt, nil
			default:
				return consumed, doubt, readErr
			}
			break
		}
	}
}
