// Package trace records what the daemon saw, so that any policy can be replayed against a real
// day offline (see manager.Replay). A trace is gzipped JSON lines, one Frame each: a sweep's
// inputs, an API request, an agent exit notice, a Docker origin. Process tables are delta
// encoded, so a day of 30-second sweeps stays a few megabytes.
//
// Traces stay on this Mac (~/.agentrm/traces, mode 0600). They hold command lines, with
// anything that looks like a secret replaced, and no environments.
package trace

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"agentrm/internal/api"
	"agentrm/internal/model"
	"agentrm/internal/sys"
)

// Facts is who a request came from, as the process table showed it: replay re-resolves it.
type Facts struct {
	PID      int    `json:"pid,omitempty"`
	AgentPID int    `json:"agent_pid,omitempty"`
	Agent    string `json:"agent,omitempty"`
	GUI      bool   `json:"gui,omitempty"`
	App      string `json:"app,omitempty"`
	AppPID   int    `json:"app_pid,omitempty"`
	ViaApp   bool   `json:"via_app,omitempty"`
}

// Owner is what an agent's environment says about a process (no environment is recorded).
type Owner struct {
	Agent    string `json:"agent,omitempty"`
	Session  string `json:"session,omitempty"`
	AgentPID int    `json:"agent_pid,omitempty"`
}

// Procs is a process table, whole or as a change from the previous one.
type Procs struct {
	Full  bool              `json:"full,omitempty"`
	Set   map[int]*sys.Proc `json:"set,omitempty"`  // new or changed
	Gone  []int             `json:"gone,omitempty"` // exited
	Owner map[int]Owner     `json:"owner,omitempty"`
	// Footprint is the compressed-inclusive size of processes where it differs from RSS by
	// more than a little (the collector sizes leftovers by it).
	Footprint map[int]float64 `json:"fp,omitempty"`
}

// Sweep is everything a full sweep reads from the machine.
type Sweep struct {
	Procs  *Procs            `json:"procs,omitempty"`
	Found  []*model.Resource `json:"found"`
	Failed map[string]string `json:"failed,omitempty"`
	Mem    sys.MemStats      `json:"mem"`
	Est    map[string]bool   `json:"est,omitempty"`
	Cwds   map[int]string    `json:"cwds,omitempty"`
}

type Request struct {
	Path  string  `json:"path"`
	Facts Facts   `json:"facts"`
	Req   api.Req `json:"req"`
}

type Frame struct {
	T     time.Time `json:"t"`
	Sweep *Sweep    `json:"sweep,omitempty"`
	Fast  *Sweep    `json:"fast,omitempty"` // a fast tick: memory and processes only
	// Mem: memory read again for a request while a tick was under way (peekMem).
	Mem     *sys.MemStats `json:"mem,omitempty"`
	Request *Request      `json:"req,omitempty"`
	Exit    int           `json:"exit,omitempty"` // an agent PID the kernel said exited
	// Decision: the live daemon decided to pause, stop or reclaim (replay checks it agrees).
	Decision *model.Decision `json:"decision,omitempty"`
	ExitTab  *Procs          `json:"exit_procs,omitempty"`
	// Origin: Docker's event stream credited a container to a session.
	Origin *struct {
		ID      string `json:"id"`
		Session string `json:"session"`
	} `json:"origin,omitempty"`
}

// ---- secrets ----

var secretArg = regexp.MustCompile(`(?i)((?:token|secret|passw(?:or)?d|api[_-]?key|access[_-]?key|auth|bearer|credential)s?[=: ]+)\S+`)
var longOpaque = regexp.MustCompile(`\b[A-Za-z0-9_\-+/]{32,}={0,2}\b`)

// Redact replaces what looks like a secret in a command line.
func Redact(s string) string {
	s = secretArg.ReplaceAllString(s, "${1}[redacted]")
	return longOpaque.ReplaceAllStringFunc(s, func(m string) string {
		if strings.Count(m, "/") > 2 {
			return m // a path, not a token
		}
		return "[redacted]"
	})
}

// ---- writing ----

// Writer writes frames to gzipped files: a new one each day and each time the daemon starts,
// so a crash can only cut short the end of one file, which the reader tolerates.
type Writer struct {
	dir  string
	keep int

	mu    sync.Mutex
	day   string
	f     *os.File
	gz    *gzip.Writer
	bw    *bufio.Writer
	last  map[int]*sys.Proc
	fp    map[int]float64
	owned map[int]bool // owners already written
}

func NewWriter(dir string, keepDays int) *Writer {
	return &Writer{dir: dir, keep: keepDays}
}

func Dir(home string) string { return filepath.Join(home, "traces") }

func (w *Writer) open(t time.Time) error {
	day := t.Format("2006-01-02")
	if w.f != nil && day == w.day {
		return nil
	}
	w.closeLocked()
	if err := os.MkdirAll(w.dir, 0o700); err != nil {
		return err
	}
	name := fmt.Sprintf("%s-%s.jsonl.gz", day, t.Format("150405"))
	f, err := os.OpenFile(filepath.Join(w.dir, name), os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	w.f, w.day = f, day
	w.gz = gzip.NewWriter(f)
	w.bw = bufio.NewWriterSize(w.gz, 64<<10)
	w.last, w.fp, w.owned = nil, nil, nil // a new file starts with a whole table
	w.prune()
	return nil
}

// prune removes files older than the days kept.
func (w *Writer) prune() {
	files, _ := filepath.Glob(filepath.Join(w.dir, "*.jsonl.gz"))
	cutoff := time.Now().AddDate(0, 0, -w.keep).Format("2006-01-02")
	for _, f := range files {
		if base := filepath.Base(f); len(base) >= 10 && base[:10] < cutoff {
			_ = os.Remove(f)
		}
	}
}

// cpuBand puts CPU into the bands decisions look at (idle, a little, busy, very busy).
func cpuBand(c float64) float64 {
	for _, b := range []float64{100, 50, 25, 10, 5, 3, 2, 1} {
		if c >= b {
			return b
		}
	}
	return 0
}

// same says whether two samples of a process differ in nothing a decision depends on: memory
// within 5% (or 20 MB), CPU in the same band.
func same(a, b *sys.Proc) bool {
	if a.PID != b.PID || a.PPID != b.PPID || !a.Start.Equal(b.Start) || a.Exe != b.Exe || a.Path != b.Path || a.Args != b.Args || a.CPU != b.CPU {
		return false
	}
	d := math.Abs(a.RSSMB - b.RSSMB)
	return d <= 20 && d <= 0.05*max(a.RSSMB, b.RSSMB)+1
}

// Delta encodes a table against the previous one written. A process whose sample did not
// change in anything a decision depends on is not written again.
func (w *Writer) delta(procs map[int]*sys.Proc, owner func(*sys.Proc) (Owner, bool), footprint func(int) (float64, bool)) *Procs {
	out := &Procs{Set: map[int]*sys.Proc{}, Owner: map[int]Owner{}, Footprint: map[int]float64{}}
	if w.last == nil {
		out.Full = true
		w.last, w.fp, w.owned = map[int]*sys.Proc{}, map[int]float64{}, map[int]bool{}
	}
	for pid, p := range procs {
		q := *p
		q.RSSMB, q.CPU = math.Round(q.RSSMB), cpuBand(q.CPU)
		q.Args = Redact(q.Args)
		old := w.last[pid]
		if old != nil && !old.Start.Equal(q.Start) {
			delete(w.owned, pid) // a new process with a reused PID
		}
		if old == nil || !same(old, &q) {
			out.Set[pid] = &q
			w.last[pid] = &q
		}
		if !w.owned[pid] {
			if o, ok := owner(p); ok {
				out.Owner[pid] = o
				w.owned[pid] = true
			}
		}
		if footprint != nil && !systemPath(p.Exe) {
			if fp, ok := footprint(pid); ok && fp > q.RSSMB*1.2+8 {
				fp = math.Round(fp)
				if w.fp[pid] != fp {
					out.Footprint[pid] = fp
					w.fp[pid] = fp
				}
			}
		}
	}
	for pid := range w.last {
		if procs[pid] == nil {
			out.Gone = append(out.Gone, pid)
			delete(w.last, pid)
			delete(w.fp, pid)
			delete(w.owned, pid)
		}
	}
	return out
}

func systemPath(exe string) bool {
	return strings.HasPrefix(exe, "/System/") || strings.HasPrefix(exe, "/usr/") || strings.HasPrefix(exe, "/sbin/") || strings.HasPrefix(exe, "/bin/")
}

// Procs records a table as a delta. It must be called under the writer's ordering: only one
// goroutine (the daemon's sweep loop) writes tables.
func (w *Writer) Procs(t time.Time, procs map[int]*sys.Proc, owner func(*sys.Proc) (Owner, bool), footprint func(int) (float64, bool)) *Procs {
	if procs == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.open(t); err != nil {
		return nil
	}
	return w.delta(procs, owner, footprint)
}

// Write appends one frame.
func (w *Writer) Write(f Frame) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.open(f.T); err != nil {
		return
	}
	b, err := json.Marshal(f)
	if err != nil {
		return
	}
	_, _ = w.bw.Write(append(b, '\n'))
}

// Flush makes everything written so far readable.
func (w *Writer) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.bw != nil {
		_ = w.bw.Flush()
		_ = w.gz.Flush()
	}
}

func (w *Writer) closeLocked() {
	if w.f == nil {
		return
	}
	_ = w.bw.Flush()
	_ = w.gz.Close()
	_ = w.f.Close()
	w.f, w.gz, w.bw = nil, nil, nil
}

func (w *Writer) Close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closeLocked()
}

// ---- reading ----

// Reader yields a trace's frames in order, with each process table made whole again.
type Reader struct {
	files []string
	cur   io.ReadCloser
	gz    *gzip.Reader
	sc    *bufio.Scanner
	table map[int]*sys.Proc
	owner map[int]Owner
	fp    map[int]float64
}

// Open reads one trace file, or every file in a directory in date order.
func Open(path string) (*Reader, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	files := []string{path}
	if fi.IsDir() {
		files, _ = filepath.Glob(filepath.Join(path, "*.jsonl.gz"))
		sort.Strings(files)
		if len(files) == 0 {
			return nil, fmt.Errorf("no traces in %s", path)
		}
	}
	return &Reader{files: files}, nil
}

func (r *Reader) next() error {
	if r.cur != nil {
		_ = r.cur.Close()
		r.cur = nil
	}
	if len(r.files) == 0 {
		return io.EOF
	}
	f, err := os.Open(r.files[0])
	r.files = r.files[1:]
	if err != nil {
		return err
	}
	gz, err := gzip.NewReader(f)
	if err != nil {
		_ = f.Close()
		return err
	}
	r.cur, r.gz = f, gz
	r.sc = bufio.NewScanner(gz)
	r.sc.Buffer(make([]byte, 1<<20), 64<<20)
	return nil
}

// Apply turns a recorded delta into the whole table it describes.
func (r *Reader) Apply(p *Procs) {
	if p == nil {
		return
	}
	if p.Full || r.table == nil {
		r.table, r.owner, r.fp = map[int]*sys.Proc{}, map[int]Owner{}, map[int]float64{}
	}
	for _, pid := range p.Gone {
		delete(r.table, pid)
		delete(r.owner, pid)
		delete(r.fp, pid)
	}
	for pid, q := range p.Set {
		if old := r.table[pid]; old != nil && !old.Start.Equal(q.Start) {
			delete(r.owner, pid)
			delete(r.fp, pid)
		}
		r.table[pid] = q
	}
	for pid, o := range p.Owner {
		r.owner[pid] = o
	}
	for pid, fp := range p.Footprint {
		r.fp[pid] = fp
	}
}

// Table is the whole process table as of the last frame read (a copy).
func (r *Reader) Table() map[int]*sys.Proc {
	out := make(map[int]*sys.Proc, len(r.table))
	for pid, p := range r.table {
		q := *p
		out[pid] = &q
	}
	return out
}

func (r *Reader) Owner(pid int) (Owner, bool) {
	o, ok := r.owner[pid]
	return o, ok
}

func (r *Reader) Footprint(pid int) (float64, bool) {
	fp, ok := r.fp[pid]
	if !ok {
		if p := r.table[pid]; p != nil {
			return p.RSSMB, true
		}
	}
	return fp, ok
}

// Next returns the next frame, io.EOF at the end. Tables in it are applied before it returns.
func (r *Reader) Next() (*Frame, error) {
	for {
		if r.sc == nil {
			if err := r.next(); err != nil {
				return nil, err
			}
		}
		if r.sc.Scan() {
			var f Frame
			if err := json.Unmarshal(r.sc.Bytes(), &f); err != nil {
				continue // a line cut short by a crash
			}
			if f.Sweep != nil {
				r.Apply(f.Sweep.Procs)
			}
			if f.Fast != nil {
				r.Apply(f.Fast.Procs)
			}
			r.Apply(f.ExitTab)
			return &f, nil
		}
		r.sc = nil // this file is done (a truncated gzip tail is fine)
	}
}
