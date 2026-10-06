package hook

import (
	"bufio"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"time"
)

// Event kinds in a session log.
const (
	kindStart    = "start"
	kindPrompt   = "prompt"
	kindResponse = "response"
	kindCompact  = "compact"
)

// event is one line of a session log (JSONL).
type event struct {
	Time    time.Time `json:"t"`
	Kind    string    `json:"k"`
	Text    string    `json:"x,omitempty"`
	Project string    `json:"p,omitempty"`
	Branch  string    `json:"b,omitempty"`
}

// pending is a spooled remember call.
type pending struct {
	Project     string    `json:"project"`
	Content     string    `json:"content"`
	Type        string    `json:"type"`
	SessionID   string    `json:"session_id"`
	CreatedAt   time.Time `json:"created_at"`
	Destination string    `json:"destination,omitempty"` // endpoint + identity fingerprint, never a credential
}

// state is the hook's local directory:
//
//	sessions/<hash>.jsonl   append-only log per agent session
//	spool/<time>-<rand>.json summaries waiting to be sent
//
// Files are private to the user (0600, directories 0700).
type state struct{ dir string }

func (s state) sessionsDir() string { return filepath.Join(s.dir, "sessions") }
func (s state) spoolDir() string    { return filepath.Join(s.dir, "spool") }

// sessionFile maps a client session id (arbitrary text) to a safe file name.
func (s state) sessionFile(sessionID string) string {
	h := sha256.Sum256([]byte(sessionID))
	return filepath.Join(s.sessionsDir(), hex.EncodeToString(h[:12])+".jsonl")
}

// append adds e to the session log with a single O_APPEND write, so events
// from concurrent hook processes do not interleave.
func (s state) append(sessionID string, e event) error {
	if err := os.MkdirAll(s.sessionsDir(), 0o700); err != nil {
		return err
	}
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(s.sessionFile(sessionID), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.Write(append(line, '\n'))
	cerr := f.Close()
	return errors.Join(werr, cerr)
}

// read returns the session's events; a missing log yields none. Corrupt lines
// (e.g. from a crash mid-write) are skipped.
func (s state) read(sessionID string) ([]event, error) {
	f, err := os.Open(s.sessionFile(sessionID))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		var e event
		if json.Unmarshal(sc.Bytes(), &e) == nil && e.Kind != "" {
			out = append(out, e)
		}
	}
	return out, sc.Err()
}

// spool writes p atomically (temp file + rename) and returns its path.
func (s state) spool(p pending) (string, error) {
	if err := os.MkdirAll(s.spoolDir(), 0o700); err != nil {
		return "", err
	}
	b, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	var rnd [4]byte
	_, _ = rand.Read(rnd[:])
	name := strconv.FormatInt(p.CreatedAt.UnixNano(), 10) + "-" + hex.EncodeToString(rnd[:]) + ".json"
	final := filepath.Join(s.spoolDir(), name)
	tmp, err := os.CreateTemp(s.spoolDir(), ".tmp-*")
	if err != nil {
		return "", err
	}
	_, werr := tmp.Write(b)
	cerr := tmp.Close()
	if err := errors.Join(werr, cerr); err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	if err := os.Rename(tmp.Name(), final); err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	return final, nil
}

// spooled lists spooled summaries, oldest first.
func (s state) spooled() []string {
	files, _ := filepath.Glob(filepath.Join(s.spoolDir(), "*.json"))
	slices.Sort(files) // names start with a nanosecond timestamp
	return files
}

func (s state) load(path string) (pending, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return pending{}, err
	}
	var p pending
	if err := json.Unmarshal(b, &p); err != nil {
		return pending{}, err
	}
	if p.Project == "" || p.Content == "" || p.Type == "" {
		return pending{}, fmt.Errorf("incomplete spooled summary")
	}
	return p, nil
}

func (s state) unspool(path string) { _ = os.Remove(path) }

// cleanup removes session logs and spooled summaries that are too old to be
// useful. Session logs are kept for a while after SessionEnd so that a resumed
// session's summary covers the whole session.
func (s state) cleanup(now time.Time) {
	remove := func(pattern string, maxAge time.Duration) {
		files, _ := filepath.Glob(pattern)
		for _, f := range files {
			if fi, err := os.Stat(f); err == nil && now.Sub(fi.ModTime()) > maxAge {
				_ = os.Remove(f)
			}
		}
	}
	remove(filepath.Join(s.sessionsDir(), "*.jsonl"), sessionMaxAge)
	remove(filepath.Join(s.spoolDir(), "*.json"), spoolMaxAge)
	remove(filepath.Join(s.spoolDir(), ".tmp-*"), time.Hour)
}
