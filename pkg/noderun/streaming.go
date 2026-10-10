package noderun

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ExecuteStreaming writes live output without changing the legacy node runner.
type ExecuteStreaming func(context.Context, string, string, int64, io.Writer, io.Writer) Result

func NewStreaming(dir string, execute ExecuteStreaming, started func(Job, Request) error, finished func(Job)) (*Service, error) {
	s, err := New(dir, nil, finished)
	if err != nil {
		return nil, err
	}
	s.streaming = execute
	s.started = started
	if finished != nil {
		for _, job := range s.recovered {
			finished(job)
		}
	}
	s.recovered = nil
	return s, nil
}

type cappedOutput struct {
	mu        sync.Mutex
	file      *os.File
	written   int
	truncated bool
}

func (w *cappedOutput) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	total := len(p)
	keep := min(total, MaxOutputBytes-w.written)
	if keep > 0 {
		n, err := w.file.Write(p[:keep])
		w.written += n
		if err != nil {
			return n, err
		}
	}
	if keep < total {
		w.truncated = true
	}
	return total, nil
}
func (s *Service) executeStreaming(ctx context.Context, j Job) Result {
	dir := filepath.Join(s.dir, j.ID)
	stdout, err := os.OpenFile(filepath.Join(dir, "stdout.log"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return Result{ExitCode: 125, Error: "stdout unavailable"}
	}
	defer func() { _ = stdout.Close() }()
	stderr, err := os.OpenFile(filepath.Join(dir, "stderr.log"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return Result{ExitCode: 125, Error: "stderr unavailable"}
	}
	defer func() { _ = stderr.Close() }()
	out, errout := &cappedOutput{file: stdout}, &cappedOutput{file: stderr}
	result := s.streaming(ctx, filepath.Join(dir, "script.sh"), j.ID, j.TimeoutSeconds, out, errout)
	if err := errors.Join(stdout.Sync(), stderr.Sync()); err != nil {
		result.ExitCode = 125
		result.Error = "script output persistence failed"
	}
	result.OutputTruncated = out.truncated || errout.truncated
	// Keep final metadata consistent with legacy jobs; stored output is bounded.
	raw, _ := readStreamingOutput(filepath.Join(dir, "stdout.log"))
	result.Stdout = string(raw)
	raw, _ = readStreamingOutput(filepath.Join(dir, "stderr.log"))
	result.Stderr = string(raw)
	return result
}
func (s *Service) refreshStreamingJob(j *Job) {
	if j.State == "running" {
		j.DurationMS = time.Since(j.CreatedAt).Milliseconds()
	}
	for _, stream := range []string{"stdout", "stderr"} {
		f, err := os.Open(filepath.Join(s.dir, j.ID, stream+".log"))
		if err != nil {
			continue
		}
		info, err := f.Stat()
		if err != nil {
			_ = f.Close()
			continue
		}
		size := min(info.Size(), int64(MaxOutputBytes))
		j.OutputTruncated = j.OutputTruncated || info.Size() > int64(MaxOutputBytes)
		offset := max(int64(0), size-TailBytes)
		raw := make([]byte, size-offset)
		_, _ = f.ReadAt(raw, offset)
		_ = f.Close()
		if stream == "stdout" {
			j.StdoutTail = string(raw)
			j.StdoutBytes = size
		} else {
			j.StderrTail = string(raw)
			j.StderrBytes = size
		}
	}
}

func readStreamingOutput(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return io.ReadAll(io.LimitReader(f, MaxOutputBytes))
}
