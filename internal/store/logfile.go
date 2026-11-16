package store

import (
	"io"
	"os"
	"strconv"
	"sync"
)

// RotatingFile is an append-only log file that rolls over at a size limit, keeping a few old
// files (path.1 is the newest of them).
type RotatingFile struct {
	path  string
	limit int64
	keep  int
	mu    sync.Mutex
	f     *os.File
	size  int64
}

var _ io.Writer = (*RotatingFile)(nil)

func NewRotatingFile(path string, limit int64, keep int) *RotatingFile {
	return &RotatingFile{path: path, limit: limit, keep: keep}
}

func (r *RotatingFile) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		if err := r.open(); err != nil {
			return 0, err
		}
	}
	if r.size+int64(len(p)) > r.limit {
		r.rotate()
	}
	n, err := r.f.Write(p)
	r.size += int64(n)
	return n, err
}

func (r *RotatingFile) open() error {
	f, err := os.OpenFile(r.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	fi, _ := f.Stat()
	r.f, r.size = f, 0
	if fi != nil {
		r.size = fi.Size()
	}
	return nil
}

func (r *RotatingFile) rotate() {
	if r.f != nil {
		r.f.Close()
	}
	for i := r.keep - 1; i >= 1; i-- {
		_ = os.Rename(r.path+"."+strconv.Itoa(i), r.path+"."+strconv.Itoa(i+1))
	}
	_ = os.Rename(r.path, r.path+".1")
	_ = r.open()
}

func (r *RotatingFile) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		return nil
	}
	return r.f.Close()
}
