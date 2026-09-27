// Package apikey manages Kenfold API keys. Each key belongs to exactly one agent
// (e.g. "claude-code"); requests authenticated with the key are attributed to
// that agent, which is how memory.source_agent is set reliably over stateless
// HTTP.
//
// Keys are 256-bit random values rendered as "kf_" + base64url (46 characters).
// Only their SHA-256 hash is stored. A fast hash is appropriate because the keys
// are uniformly random, not user-chosen passwords.
package apikey

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/modelcontextprotocol/go-sdk/auth"

	"github.com/kenfold/kenfold/internal/memory"
)

const (
	// Prefix starts every key, so leaked keys are recognizable by secret scanners.
	Prefix = "kf_"
	// keyBytes of randomness per key.
	keyBytes = 32
	// KeyLen is the length of a key string.
	KeyLen = len(Prefix) + 43 // base64url(32 bytes), unpadded
	// displayLen is how much of the key is stored and shown to identify it.
	displayLen = len(Prefix) + 8
	// ExtraAgent is the TokenInfo.Extra key that carries the agent name.
	ExtraAgent = "kenfold.agent"
	// ExtraKeyID is the TokenInfo.Extra key that carries the API key id.
	ExtraKeyID = "kenfold.key_id"
)

var (
	// ErrNotFound means no key matched.
	ErrNotFound = errors.New("api key not found")
	// ErrAmbiguous means a prefix matched more than one key.
	ErrAmbiguous = errors.New("api key prefix matches more than one key; use the id")
)

// Key is an API key record. The plaintext key is never stored.
type Key struct {
	ID         string
	Agent      string
	Prefix     string
	CreatedAt  time.Time
	LastUsedAt *time.Time
	RevokedAt  *time.Time
}

// Store persists API keys.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore returns a Store backed by pool.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// Generate returns a new random key.
func Generate() (string, error) {
	b := make([]byte, keyBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate api key: %w", err)
	}
	return Prefix + base64.RawURLEncoding.EncodeToString(b), nil
}

// WellFormed reports whether s has the shape of a Kenfold key. It lets the
// server reject garbage without a database round trip.
func WellFormed(s string) bool {
	if len(s) != KeyLen || !strings.HasPrefix(s, Prefix) {
		return false
	}
	for _, c := range s[len(Prefix):] {
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// Hash returns the stored form of a key.
func Hash(key string) []byte {
	h := sha256.Sum256([]byte(key))
	return h[:]
}

// Create generates and stores a key for agent. The plaintext is returned once
// and cannot be recovered later.
func (s *Store) Create(ctx context.Context, agent string) (string, Key, error) {
	if !memory.ValidAgent(agent) {
		return "", Key{}, fmt.Errorf("invalid agent name %q: use lowercase letters, digits, '.', '_' or '-', e.g. claude-code", agent)
	}
	plain, err := Generate()
	if err != nil {
		return "", Key{}, err
	}
	var k Key
	err = s.pool.QueryRow(ctx, `
		INSERT INTO api_key (agent, prefix, key_hash) VALUES ($1, $2, $3)
		RETURNING `+keyColumns, agent, plain[:displayLen], Hash(plain)).Scan(keyFields(&k)...)
	if err != nil {
		return "", Key{}, fmt.Errorf("create api key: %w", err)
	}
	return plain, k, nil
}

// List returns keys, newest first. Revoked keys are included only if asked.
func (s *Store) List(ctx context.Context, includeRevoked bool) ([]Key, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+keyColumns+` FROM api_key
		WHERE $1 OR revoked_at IS NULL
		ORDER BY created_at DESC, id DESC`, includeRevoked)
	if err != nil {
		return nil, fmt.Errorf("list api keys: %w", err)
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Key, error) {
		var k Key
		return k, r.Scan(keyFields(&k)...)
	})
}

// Revoke revokes the key identified by ref, which is either the key id or its
// displayed prefix (e.g. "kf_AbCdEfGh"). Revoking an already revoked key is a
// no-op that returns the key.
func (s *Store) Revoke(ctx context.Context, ref string) (Key, error) {
	ref = strings.TrimSpace(ref)
	var k Key
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id FROM api_key
			WHERE id::text = $1 OR prefix = $1
			FOR UPDATE`, ref)
		if err != nil {
			return err
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		switch len(ids) {
		case 0:
			return ErrNotFound
		case 1:
		default:
			return ErrAmbiguous
		}
		return tx.QueryRow(ctx, `
			UPDATE api_key SET revoked_at = coalesce(revoked_at, now())
			WHERE id = $1
			RETURNING `+keyColumns, ids[0]).Scan(keyFields(&k)...)
	})
	if err != nil {
		return Key{}, err
	}
	return k, nil
}

// Verify returns the active key matching plaintext key, or ErrNotFound. It
// records last use at most once a minute to avoid a write per request.
func (s *Store) Verify(ctx context.Context, key string) (Key, error) {
	if !WellFormed(key) {
		return Key{}, ErrNotFound
	}
	var k Key
	err := s.pool.QueryRow(ctx, `
		WITH k AS (
		    SELECT `+keyColumns+` FROM api_key
		    WHERE key_hash = $1 AND revoked_at IS NULL
		), touch AS (
		    UPDATE api_key SET last_used_at = now()
		    WHERE id IN (SELECT id FROM k)
		      AND (last_used_at IS NULL OR last_used_at < now() - interval '1 minute')
		)
		SELECT `+keyColumns+` FROM k`, Hash(key)).Scan(keyFields(&k)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return Key{}, ErrNotFound
	}
	if err != nil {
		return Key{}, fmt.Errorf("verify api key: %w", err)
	}
	return k, nil
}

// TokenVerifier adapts Verify to the MCP SDK's bearer-token middleware. The
// agent name and key id are carried in TokenInfo.Extra. Keys do not expire
// (they are revoked instead), so the middleware must allow a missing expiration.
// Database errors are logged and reported to the client without details.
func (s *Store) TokenVerifier(logger *slog.Logger) auth.TokenVerifier {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return func(ctx context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
		k, err := s.Verify(ctx, token)
		if errors.Is(err, ErrNotFound) {
			return nil, fmt.Errorf("%w: unknown or revoked API key", auth.ErrInvalidToken)
		}
		if err != nil {
			// 500, not 401: the key may be valid but the database is unavailable.
			logger.ErrorContext(ctx, "api key verification failed", "err", err)
			return nil, errors.New("api key verification is temporarily unavailable")
		}
		return &auth.TokenInfo{
			UserID: k.ID,
			Extra:  map[string]any{ExtraAgent: k.Agent, ExtraKeyID: k.ID},
		}, nil
	}
}

// AgentFrom returns the agent name carried by ti, if any.
func AgentFrom(ti *auth.TokenInfo) (string, bool) {
	if ti == nil {
		return "", false
	}
	a, ok := ti.Extra[ExtraAgent].(string)
	return a, ok && a != ""
}

const keyColumns = `id, agent, prefix, created_at, last_used_at, revoked_at`

func keyFields(k *Key) []any {
	return []any{&k.ID, &k.Agent, &k.Prefix, &k.CreatedAt, &k.LastUsedAt, &k.RevokedAt}
}
