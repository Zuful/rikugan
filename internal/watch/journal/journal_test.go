package journal

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)

type clock struct{ now time.Time }

func (c *clock) Now() time.Time { return c.now }

func replayAll(t *testing.T, j *Journal) ([]Record, int) {
	t.Helper()
	var out []Record
	skipped, err := j.Replay(func(r Record) error {
		out = append(out, r)
		return nil
	})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	return out, skipped
}

func kinds(rs []Record) string {
	var k []string
	for _, r := range rs {
		k = append(k, r.Kind)
	}
	return strings.Join(k, ",")
}

func TestAppendThenReplayAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	c := &clock{now: t0}
	j, err := Open(dir, Options{Now: c.Now})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(map[string]string{"verdict": "ok"})
	for _, r := range []Record{
		{Kind: "verdict", Service: "shop/checkout", Data: data},
		{Kind: "notification", Service: "shop/checkout", Time: t0.Add(time.Minute)},
	} {
		if err := j.Append(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}

	j, err = Open(dir, Options{Now: c.Now})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = j.Close() }()
	got, skipped := replayAll(t, j)
	if kinds(got) != "verdict,notification" || skipped != 0 {
		t.Fatalf("replayed %q (skipped %d)", kinds(got), skipped)
	}
	if !got[0].Time.Equal(t0) {
		t.Errorf("zero time not stamped with the clock: %v", got[0].Time)
	}
	if string(got[0].Data) != `{"verdict":"ok"}` || got[0].Service != "shop/checkout" {
		t.Errorf("record not preserved: %+v", got[0])
	}
}

func TestAppendRejectsRecordWithoutKind(t *testing.T) {
	j, err := Open(t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = j.Close() }()
	if err := j.Append(Record{Service: "x"}); err == nil {
		t.Error("record without a kind accepted")
	}
}

// A crash in the middle of a write leaves a truncated last line. Open
// must terminate it so the next record is readable, and Replay must skip
// the fragment.
func TestCrashMidWriteIsRepaired(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	content := `{"time":"2026-10-18T12:00:00Z","kind":"verdict"}` + "\n" + `{"time":"2026-10-18T12:01:00Z","ki`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	j, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = j.Close() }()
	if err := j.Append(Record{Kind: "notification", Time: t0}); err != nil {
		t.Fatal(err)
	}
	got, skipped := replayAll(t, j)
	if kinds(got) != "verdict,notification" || skipped != 1 {
		t.Errorf("replayed %q, skipped %d; want verdict,notification and 1 skipped", kinds(got), skipped)
	}
}

func TestRotationKeepsOrderAndRetentionPrunes(t *testing.T) {
	dir := t.TempDir()
	c := &clock{now: t0}
	// Each record is ~50 bytes: a 120-byte limit rotates every 2 records.
	j, err := Open(dir, Options{MaxSize: 120, Retention: 48 * time.Hour, Now: c.Now})
	if err != nil {
		t.Fatal(err)
	}
	for i := range 6 {
		if err := j.Append(Record{Kind: string(rune('a' + i))}); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := replayAll(t, j)
	if kinds(got) != "a,b,c,d,e,f" {
		t.Fatalf("replayed %q across rotations, want a..f in order", kinds(got))
	}
	rotated, _ := j.rotatedFiles()
	if len(rotated) != 2 {
		t.Fatalf("%d rotated files, want 2 (frozen clock must not overwrite)", len(rotated))
	}

	// Three days later, the next rotation prunes everything rotated at t0.
	c.now = t0.Add(72 * time.Hour)
	for i := range 2 {
		if err := j.Append(Record{Kind: string(rune('g' + i))}); err != nil {
			t.Fatal(err)
		}
	}
	got, _ = replayAll(t, j)
	// a..d were rotated at t0 and are pruned; e,f were rotated just now.
	if kinds(got) != "e,f,g,h" {
		t.Fatalf("after retention, replayed %q, want e,f,g,h", kinds(got))
	}
	_ = j.Close()
}

func TestReplayStopsOnCallbackError(t *testing.T) {
	j, err := Open(t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = j.Close() }()
	_ = j.Append(Record{Kind: "a"})
	_ = j.Append(Record{Kind: "b"})
	stop := errors.New("stop")
	n := 0
	_, err = j.Replay(func(Record) error { n++; return stop })
	if !errors.Is(err, stop) || n != 1 {
		t.Errorf("err %v after %d calls, want stop after 1", err, n)
	}
}

func TestReplaySkipsOversizedAndForeignLines(t *testing.T) {
	dir := t.TempDir()
	huge := `{"kind":"x","data":"` + strings.Repeat("a", maxLine) + `"}`
	content := "not json\n" + huge + "\n" + `{"time":"2026-10-18T12:00:00Z"}` + "\n" + `{"time":"2026-10-18T12:00:00Z","kind":"ok"}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "verdicts-notadate.jsonl"), []byte(`{"kind":"foreign"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	j, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = j.Close() }()
	got, skipped := replayAll(t, j)
	if kinds(got) != "ok" || skipped != 3 {
		t.Errorf("replayed %q, skipped %d; want ok and 3 skipped", kinds(got), skipped)
	}
}
