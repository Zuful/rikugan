// Package journal is the daemon's source of truth: an append-only JSONL
// file of records (verdicts, notifications, state snapshots) under the
// state directory, with size-based rotation and time-based retention
// (design §4).
//
// The journal has exactly one writer. The daemon guarantees it (single
// instance, lock file); this package does not try to.
package journal

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// FileName is the active journal file inside the state directory.
// Rotated files are named verdicts-<UTC timestamp>.jsonl next to it.
const FileName = "verdicts.jsonl"

const (
	rotatedPrefix = "verdicts-"
	rotatedSuffix = ".jsonl"
	rotatedLayout = "20060102T150405.000000000Z"
)

// Defaults for Options.
const (
	DefaultMaxSize   = 10 << 20 // 10 MiB
	DefaultRetention = 30 * 24 * time.Hour
)

// maxLine bounds one record when reading back. Records are small (a
// verdict and its signals); anything larger is not ours.
const maxLine = 1 << 20

// Record is one journal line. Data holds the kind-specific payload, so
// the journal stays agnostic of what the daemon records.
type Record struct {
	Time    time.Time       `json:"time"`
	Kind    string          `json:"kind"`
	Service string          `json:"service,omitempty"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// Options tunes rotation and retention. Zero values take the defaults.
type Options struct {
	// MaxSize is the size past which the active file is rotated.
	MaxSize int64
	// Retention is how long rotated files are kept, judged on the
	// time they were rotated.
	Retention time.Duration
	// Now is the clock, for tests. Defaults to time.Now.
	Now func() time.Time
}

// Journal appends records to the active file and reads them all back.
type Journal struct {
	dir  string
	opts Options
	f    *os.File
	size int64
}

// Open opens (or creates) the journal in dir and prunes rotated files
// past retention. If the daemon died in the middle of a write, the
// truncated last line is terminated so the next record starts cleanly;
// Replay skips it.
func Open(dir string, opts Options) (*Journal, error) {
	if opts.MaxSize <= 0 {
		opts.MaxSize = DefaultMaxSize
	}
	if opts.Retention <= 0 {
		opts.Retention = DefaultRetention
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("journal: create state directory: %w", err)
	}
	j := &Journal{dir: dir, opts: opts}
	if err := j.openActive(); err != nil {
		return nil, err
	}
	if err := j.prune(); err != nil {
		_ = j.f.Close()
		return nil, err
	}
	return j, nil
}

func (j *Journal) openActive() error {
	path := filepath.Join(j.dir, FileName)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600) // #nosec G304 -- fixed file name in the operator's state directory
	if err != nil {
		return fmt.Errorf("journal: open %s: %w", FileName, err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("journal: stat %s: %w", FileName, err)
	}
	size := info.Size()
	if size > 0 {
		last := make([]byte, 1)
		if _, err := f.ReadAt(last, size-1); err != nil {
			_ = f.Close()
			return fmt.Errorf("journal: read %s: %w", FileName, err)
		}
		if last[0] != '\n' {
			if _, err := f.Write([]byte{'\n'}); err != nil {
				_ = f.Close()
				return fmt.Errorf("journal: repair %s: %w", FileName, err)
			}
			size++
		}
	}
	j.f, j.size = f, size
	return nil
}

// Append writes r as one line and syncs it to disk before returning:
// records are rare (a few per rollout or sweep) and each one may stand
// for a notification that must not be sent twice after a crash. A zero
// Time is stamped with the journal's clock.
func (j *Journal) Append(r Record) error {
	if r.Time.IsZero() {
		r.Time = j.opts.Now()
	}
	if r.Kind == "" {
		return errors.New("journal: record without a kind")
	}
	line, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("journal: encode record: %w", err)
	}
	line = append(line, '\n')
	if j.size > 0 && j.size+int64(len(line)) > j.opts.MaxSize {
		if err := j.rotate(); err != nil {
			return err
		}
	}
	if _, err := j.f.Write(line); err != nil {
		return fmt.Errorf("journal: write: %w", err)
	}
	j.size += int64(len(line))
	if err := j.f.Sync(); err != nil {
		return fmt.Errorf("journal: sync: %w", err)
	}
	return nil
}

// rotate renames the active file to a timestamped one, starts a new
// active file, and prunes what retention no longer covers.
func (j *Journal) rotate() error {
	if err := j.f.Close(); err != nil {
		return fmt.Errorf("journal: close before rotation: %w", err)
	}
	// Never overwrite an earlier rotation, even with a coarse or frozen
	// clock: rename would silently replace it.
	at := j.opts.Now().UTC()
	name := rotatedPrefix + at.Format(rotatedLayout) + rotatedSuffix
	for {
		if _, err := os.Stat(filepath.Join(j.dir, name)); errors.Is(err, os.ErrNotExist) {
			break
		}
		at = at.Add(time.Nanosecond)
		name = rotatedPrefix + at.Format(rotatedLayout) + rotatedSuffix
	}
	if err := os.Rename(filepath.Join(j.dir, FileName), filepath.Join(j.dir, name)); err != nil {
		return fmt.Errorf("journal: rotate: %w", err)
	}
	if err := j.openActive(); err != nil {
		return err
	}
	return j.prune()
}

// prune removes rotated files older than the retention period.
func (j *Journal) prune() error {
	rotated, err := j.rotatedFiles()
	if err != nil {
		return err
	}
	cutoff := j.opts.Now().Add(-j.opts.Retention)
	for _, rf := range rotated {
		if rf.at.Before(cutoff) {
			if err := os.Remove(filepath.Join(j.dir, rf.name)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("journal: prune %s: %w", rf.name, err)
			}
		}
	}
	return nil
}

type rotatedFile struct {
	name string
	at   time.Time
}

// rotatedFiles lists rotated files oldest first. Files that only look
// like ours are ignored.
func (j *Journal) rotatedFiles() ([]rotatedFile, error) {
	entries, err := os.ReadDir(j.dir)
	if err != nil {
		return nil, fmt.Errorf("journal: list state directory: %w", err)
	}
	var out []rotatedFile
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, rotatedPrefix) || !strings.HasSuffix(name, rotatedSuffix) {
			continue
		}
		at, err := time.Parse(rotatedLayout, strings.TrimSuffix(strings.TrimPrefix(name, rotatedPrefix), rotatedSuffix))
		if err != nil {
			continue
		}
		out = append(out, rotatedFile{name: name, at: at})
	}
	sort.Slice(out, func(a, b int) bool { return out[a].at.Before(out[b].at) })
	return out, nil
}

// Replay calls fn for every record, oldest first: rotated files in
// rotation order, then the active file. Lines that do not decode (a
// write cut short by a crash) are skipped and counted. An error from fn
// stops the replay and is returned as is.
func (j *Journal) Replay(fn func(Record) error) (skipped int, err error) {
	rotated, err := j.rotatedFiles()
	if err != nil {
		return 0, err
	}
	names := make([]string, 0, len(rotated)+1)
	for _, rf := range rotated {
		names = append(names, rf.name)
	}
	names = append(names, FileName)
	for _, name := range names {
		n, err := replayFile(filepath.Join(j.dir, name), fn)
		skipped += n
		if err != nil {
			return skipped, err
		}
	}
	return skipped, nil
}

func replayFile(path string, fn func(Record) error) (skipped int, err error) {
	f, err := os.Open(path) // #nosec G304 -- path is built from the operator's state directory and our own file names
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil // pruned between listing and reading
		}
		return 0, fmt.Errorf("journal: open %s: %w", filepath.Base(path), err)
	}
	defer func() { _ = f.Close() }()

	r := bufio.NewReaderSize(f, 64<<10)
	for {
		line, readErr := readLine(r)
		if len(bytes.TrimSpace(line)) > 0 {
			var rec Record
			if json.Unmarshal(line, &rec) != nil || rec.Kind == "" {
				skipped++
			} else if err := fn(rec); err != nil {
				return skipped, err
			}
		}
		if readErr == io.EOF {
			return skipped, nil
		}
		if readErr != nil {
			return skipped, fmt.Errorf("journal: read %s: %w", filepath.Base(path), readErr)
		}
	}
}

// readLine returns the next line without its newline. A line longer than
// maxLine is consumed and returned empty-handed as garbage.
func readLine(r *bufio.Reader) ([]byte, error) {
	var buf []byte
	for {
		chunk, err := r.ReadSlice('\n')
		if len(buf)+len(chunk) <= maxLine {
			buf = append(buf, chunk...)
		} else {
			buf = []byte("\x00") // too long: decodes as garbage
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		return bytes.TrimSuffix(buf, []byte{'\n'}), err
	}
}

// Close closes the active file.
func (j *Journal) Close() error {
	return j.f.Close()
}
