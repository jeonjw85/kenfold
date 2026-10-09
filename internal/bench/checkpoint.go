package bench

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"

	"github.com/kenfold/kenfold/internal/extract"
	"github.com/kenfold/kenfold/internal/memory"
)

// ExtractionCache stores benchmark ingestion projections, not model responses.
// Identity must pin weights, code/validation/schema/options and inference
// profile externally; a model name alone is not a reproducibility identity.
type ExtractionCache struct {
	dir, identity string
}

// ExtractionKey describes the exact session extraction being checkpointed.
type ExtractionKey struct {
	Version            int    `json:"version"`
	Identity           string `json:"identity"`
	Sample             string `json:"sample"`
	Session            string `json:"session"`
	Model              string `json:"model"`
	SourceSHA256       string `json:"source_sha256"`
	SystemPromptSHA256 string `json:"system_prompt_sha256"`
}

// ExtractionProjection is the ordered subset consumed by benchmark ingestion.
// Session and date are deliberately derived from the dataset during replay.
type ExtractionProjection struct {
	Content    string      `json:"content"`
	Type       memory.Type `json:"type"`
	Confidence float64     `json:"confidence"`
}

type extractionRecord struct {
	Key              ExtractionKey          `json:"key"`
	Memories         []ExtractionProjection `json:"memories"`
	ProjectionSHA256 string                 `json:"projection_sha256"`
}

func NewExtractionCache(dir, identity string) (*ExtractionCache, error) {
	if strings.TrimSpace(identity) == "" || strings.TrimSpace(dir) == "" {
		return nil, errors.New("extraction cache requires a directory and a nonempty pinned identity")
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("extraction cache directory: %w", err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("extraction cache directory must be a private directory, not a symlink")
	}
	return &ExtractionCache{dir: dir, identity: identity}, nil
}

// Key hashes the unmodified source passed to extract.Extract, including its
// trailing newlines, and the production system prompt.
func (c *ExtractionCache) Key(sample, session, model, source string) ExtractionKey {
	return ExtractionKey{
		Version: 1, Identity: c.identity, Sample: sample, Session: session, Model: model,
		SourceSHA256: sha256Hex([]byte(source)), SystemPromptSHA256: sha256Hex([]byte(extract.SystemPrompt)),
	}
}

// Path is also available to local recovery/seed tooling. Filenames are SHA-256
// of the compact encoding/json representation of ExtractionKey, plus ".json".
func (c *ExtractionCache) Path(key ExtractionKey) (string, error) {
	if key.Version != 1 || key.Identity != c.identity || strings.TrimSpace(key.Sample) == "" || strings.TrimSpace(key.Session) == "" || strings.TrimSpace(key.Model) == "" || !validSHA256(key.SourceSHA256) || key.SystemPromptSHA256 != sha256Hex([]byte(extract.SystemPrompt)) {
		return "", errors.New("invalid or incompatible extraction cache key")
	}
	b, err := json.Marshal(key)
	if err != nil {
		return "", err
	}
	return filepath.Join(c.dir, sha256Hex(b)+".json"), nil
}

func (c *ExtractionCache) Load(key ExtractionKey) ([]ExtractionProjection, bool, error) {
	path, err := c.Path(key)
	if err != nil {
		return nil, false, err
	}
	var rec extractionRecord
	if err := readPrivateJSON(path, &rec); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("extraction checkpoint %s: %w", path, err)
	}
	if rec.Key != key {
		return nil, false, fmt.Errorf("extraction checkpoint %s: record key mismatch", path)
	}
	if err := validateProjection(rec.Memories); err != nil {
		return nil, false, fmt.Errorf("extraction checkpoint %s: %w", path, err)
	}
	b, err := json.Marshal(rec.Memories)
	if err != nil || sha256Hex(b) != rec.ProjectionSHA256 {
		return nil, false, fmt.Errorf("extraction checkpoint %s: projection checksum mismatch", path)
	}
	return rec.Memories, true, nil
}

// Save accepts only successful projections. A nil list is a successful empty
// list; callers must never invoke Save after a failed extraction. Existing
// records are immutable and corrupt records are never silently replaced.
func (c *ExtractionCache) Save(key ExtractionKey, memories []ExtractionProjection) error {
	if memories == nil {
		memories = []ExtractionProjection{}
	}
	if err := validateProjection(memories); err != nil {
		return err
	}
	old, hit, err := c.Load(key)
	if err != nil {
		return err
	}
	if hit {
		if !reflect.DeepEqual(old, memories) {
			return errors.New("extraction checkpoint already contains a different projection")
		}
		return nil
	}
	path, err := c.Path(key)
	if err != nil {
		return err
	}
	projection, err := json.Marshal(memories)
	if err != nil {
		return err
	}
	b, err := json.Marshal(extractionRecord{Key: key, Memories: memories, ProjectionSHA256: sha256Hex(projection)})
	if err != nil {
		return err
	}
	if err := writeDurableJSON(path, b); err != nil {
		return fmt.Errorf("publish extraction checkpoint: %w", err)
	}
	return nil
}

func validateProjection(memories []ExtractionProjection) error {
	if memories == nil || len(memories) > extract.DefaultMaxCandidates {
		return errors.New("extraction projection requires a list of at most 8 memories (empty is [])")
	}
	for _, m := range memories {
		if strings.TrimSpace(m.Content) == "" || math.IsNaN(m.Confidence) || math.IsInf(m.Confidence, 0) || m.Confidence <= 0 || m.Confidence > 1 {
			return errors.New("invalid extraction projection content or confidence")
		}
		switch m.Type {
		case memory.TypeProject, memory.TypeCodebase, memory.TypePreference, memory.TypeSemantic:
		default:
			return fmt.Errorf("unsupported extraction projection type %q", m.Type)
		}
	}
	return nil
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func validSHA256(s string) bool {
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == sha256.Size && strings.ToLower(s) == s
}

// Shared by the opt-in benchmark cache and budget journal. Neither accepts
// symlinks, public files, trailing JSON, or oversized records.
func readPrivateJSON(path string, out any) error {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() > 1<<20 {
		return errors.New("record must be an owner-only regular file of at most 1 MiB")
	}
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return errors.New("record has trailing JSON or invalid trailing data")
	}
	return nil
}

// Publication is all-or-nothing, and returns only after file and directory
// fsync. Failures after rename are still surfaced; callers must stop.
func writeDurableJSON(path string, b []byte) error {
	return publishDurableJSON(path, b, false)
}

// Initial allowances must not overwrite even a concurrently appearing file.
// Linking a fully fsynced temporary inode atomically publishes it exclusively.
func writeNewDurableJSON(path string, b []byte) error {
	return publishDurableJSON(path, b, true)
}

func publishDurableJSON(path string, b []byte, exclusive bool) error {
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	f, err := os.CreateTemp(filepath.Dir(path), ".checkpoint-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := io.Copy(f, bytes.NewReader(append(b, '\n'))); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if exclusive {
		err = os.Link(f.Name(), path)
	} else {
		err = os.Rename(f.Name(), path)
	}
	if err != nil {
		return err
	}
	return dir.Sync()
}
