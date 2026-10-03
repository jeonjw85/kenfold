package oauth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kenfold/kenfold/internal/authz"
)

// Lifetimes.
const (
	codeTTL    = 5 * time.Minute
	accessTTL  = time.Hour
	refreshTTL = 30 * 24 * time.Hour
	// Failed owner-password attempts before the consent page locks, and for how long.
	maxFailedLogins = 5
	lockoutPeriod   = 15 * time.Minute
)

var (
	errNotFound     = errors.New("not found")
	errInvalid      = errors.New("invalid, expired, or already used")
	errReuse        = errors.New("refresh token reuse")
	errInvalidScope = errors.New("requested scopes exceed the refresh token's scopes")

	// Owner password checks (the OAuth consent page and the dashboard login).
	ErrWrongPassword = errors.New("wrong owner password")
	ErrOwnerLocked   = errors.New("too many failed attempts; try again in 15 minutes")
	ErrNoPassword    = errors.New("no owner password is set; run: kenfold password")

	errLocked     = ErrOwnerLocked
	errNoPassword = ErrNoPassword
)

// ownerChecks bounds concurrent password checks: each Argon2id run uses 64 MiB.
var ownerChecks = make(chan struct{}, 2)

// Store persists OAuth state.
type Store struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

// NewStore returns a Store on pool.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool, now: time.Now} }

// ---- owner ----

// SetOwnerPassword sets (or replaces) the owner password and clears any lockout.
func (s *Store) SetOwnerPassword(ctx context.Context, password string) error {
	h, err := HashPassword(password)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO oauth_owner (id, password_hash) VALUES (1, $1)
		ON CONFLICT (id) DO UPDATE SET password_hash = $1, failed_attempts = 0, locked_until = NULL`, h)
	return err
}

// OwnerPasswordSet reports whether an owner password exists.
func (s *Store) OwnerPasswordSet(ctx context.Context) (bool, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM oauth_owner`).Scan(&n)
	return n > 0, err
}

// CheckOwner verifies password with lockout: after maxFailedLogins failures
// the check refuses (errLocked) for lockoutPeriod, even for the right
// password. The row is locked so concurrent attempts are counted.
func (s *Store) CheckOwner(ctx context.Context, password string) error {
	select {
	case ownerChecks <- struct{}{}:
		defer func() { <-ownerChecks }()
	case <-ctx.Done():
		return ctx.Err()
	}
	// The failure count must be committed, so a wrong password is reported
	// after the transaction, not by returning an error from it (which would
	// roll the count back).
	wrong := false
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var hash string
		var failed int
		var lockedUntil *time.Time
		err := tx.QueryRow(ctx, `SELECT password_hash, failed_attempts, locked_until FROM oauth_owner WHERE id = 1 FOR UPDATE`).
			Scan(&hash, &failed, &lockedUntil)
		if errors.Is(err, pgx.ErrNoRows) {
			return errNoPassword
		}
		if err != nil {
			return err
		}
		now := s.now()
		if lockedUntil != nil && now.Before(*lockedUntil) {
			return errLocked
		}
		ok, err := CheckPassword(hash, password)
		if err != nil {
			return err
		}
		if ok {
			_, err = tx.Exec(ctx, `UPDATE oauth_owner SET failed_attempts = 0, locked_until = NULL WHERE id = 1`)
			return err
		}
		failed++
		var lock *time.Time
		if failed >= maxFailedLogins {
			t := now.Add(lockoutPeriod)
			lock, failed = &t, 0
		}
		wrong = true
		_, err = tx.Exec(ctx, `UPDATE oauth_owner SET failed_attempts = $1, locked_until = $2 WHERE id = 1`, failed, lock)
		return err
	})
	if err == nil && wrong {
		return ErrWrongPassword
	}
	return err
}

// OwnerStamp identifies the current owner password (a digest of its hash),
// so sessions opened with an old password can be ended when it changes.
func (s *Store) OwnerStamp(ctx context.Context) (string, error) {
	var h string
	err := s.pool.QueryRow(ctx, `SELECT password_hash FROM oauth_owner WHERE id = 1`).Scan(&h)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNoPassword
	}
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(h))
	return hex.EncodeToString(sum[:8]), nil
}

// ---- clients ----

// Client is a registered or discovered client.
type Client struct {
	ID           string
	Kind         string // "cimd" or "dcr"
	Name         string
	RedirectURIs []string
	Metadata     ClientMetadata
	FetchedAt    *time.Time
	CreatedAt    time.Time
}

func (s *Store) getClient(ctx context.Context, id string) (Client, error) {
	var c Client
	var meta []byte
	err := s.pool.QueryRow(ctx, `SELECT client_id, kind, client_name, redirect_uris, metadata, fetched_at, created_at FROM oauth_client WHERE client_id = $1`, id).
		Scan(&c.ID, &c.Kind, &c.Name, &c.RedirectURIs, &meta, &c.FetchedAt, &c.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Client{}, errNotFound
	}
	if err != nil {
		return Client{}, err
	}
	_ = json.Unmarshal(meta, &c.Metadata)
	return c, nil
}

// putClient inserts or updates a client.
func (s *Store) putClient(ctx context.Context, kind string, m *ClientMetadata, fetched bool) (Client, error) {
	meta, err := json.Marshal(m)
	if err != nil {
		return Client{}, err
	}
	var fetchedAt *time.Time
	if fetched {
		t := s.now()
		fetchedAt = &t
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO oauth_client (client_id, kind, client_name, redirect_uris, metadata, fetched_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (client_id) DO UPDATE SET client_name = $3, redirect_uris = $4, metadata = $5, fetched_at = $6`,
		m.ClientID, kind, m.ClientName, m.RedirectURIs, meta, fetchedAt)
	if err != nil {
		return Client{}, err
	}
	return s.getClient(ctx, m.ClientID)
}

// ---- grants ----

// Grant is the owner's approval of a client.
type Grant struct {
	ID         string
	ClientID   string
	ClientName string
	Agent      string
	Scopes     []string
	Resource   string
	CreatedAt  time.Time
	LastUsedAt *time.Time
	RevokedAt  *time.Time
}

func (s *Store) createGrant(ctx context.Context, clientID, agent string, scopes []string, resource string) (Grant, error) {
	var g Grant
	err := s.pool.QueryRow(ctx, `
		INSERT INTO oauth_grant (client_id, agent, scopes, resource) VALUES ($1, $2, $3, $4)
		RETURNING id, client_id, agent, scopes, resource, created_at`, clientID, agent, scopes, resource).
		Scan(&g.ID, &g.ClientID, &g.Agent, &g.Scopes, &g.Resource, &g.CreatedAt)
	return g, err
}

// Grants lists grants, newest first (revoked ones only when all is set).
func (s *Store) Grants(ctx context.Context, all bool) ([]Grant, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT g.id, g.client_id, c.client_name, g.agent, g.scopes, g.resource, g.created_at, g.last_used_at, g.revoked_at
		FROM oauth_grant g JOIN oauth_client c ON c.client_id = g.client_id
		WHERE $1 OR g.revoked_at IS NULL
		ORDER BY g.created_at DESC`, all)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Grant, error) {
		var g Grant
		return g, r.Scan(&g.ID, &g.ClientID, &g.ClientName, &g.Agent, &g.Scopes, &g.Resource, &g.CreatedAt, &g.LastUsedAt, &g.RevokedAt)
	})
}

// RevokeGrant revokes a grant (by id or unique id prefix of at least 8
// characters) and every token issued under it.
func (s *Store) RevokeGrant(ctx context.Context, ref string) (Grant, error) {
	if len(ref) < 8 {
		return Grant{}, errors.New("give the grant id or at least its first 8 characters")
	}
	var g Grant
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id FROM oauth_grant WHERE id::text LIKE $1 || '%' AND revoked_at IS NULL`, ref)
		if err != nil {
			return err
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		switch len(ids) {
		case 0:
			return errNotFound
		case 1:
		default:
			return fmt.Errorf("%q matches %d grants; give more of the id", ref, len(ids))
		}
		if err := tx.QueryRow(ctx, `UPDATE oauth_grant SET revoked_at = now() WHERE id = $1
			RETURNING id, client_id, agent, scopes, resource, created_at, last_used_at, revoked_at`, ids[0]).
			Scan(&g.ID, &g.ClientID, &g.Agent, &g.Scopes, &g.Resource, &g.CreatedAt, &g.LastUsedAt, &g.RevokedAt); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE oauth_token SET revoked_at = now() WHERE grant_id = $1 AND revoked_at IS NULL`, g.ID)
		return err
	})
	return g, err
}

// ---- codes ----

func (s *Store) createCode(ctx context.Context, grantID, redirectURI, challenge string) (string, error) {
	code, hash, err := newToken(prefixCode)
	if err != nil {
		return "", err
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO oauth_code (code_hash, grant_id, redirect_uri, code_challenge, expires_at) VALUES ($1, $2, $3, $4, $5)`,
		hash, grantID, redirectURI, challenge, s.now().Add(codeTTL))
	return code, err
}

// codeRecord is a code being redeemed.
type codeRecord struct {
	grant       Grant
	redirectURI string
	challenge   string
}

// useCode marks a code used and returns it. A code presented twice revokes
// its grant (OAuth 2.1 section 4.1.2: tokens issued with it may be stolen).
func (s *Store) useCode(ctx context.Context, code, clientID, redirectURI string) (codeRecord, error) {
	var rec codeRecord
	reused := false
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var used *time.Time
		var expires time.Time
		err := tx.QueryRow(ctx, `
			SELECT c.redirect_uri, c.code_challenge, c.expires_at, c.used_at,
			       g.id, g.client_id, g.agent, g.scopes, g.resource, g.revoked_at
			FROM oauth_code c JOIN oauth_grant g ON g.id = c.grant_id
			WHERE c.code_hash = $1 FOR UPDATE OF c`, hashToken(code)).
			Scan(&rec.redirectURI, &rec.challenge, &expires, &used,
				&rec.grant.ID, &rec.grant.ClientID, &rec.grant.Agent, &rec.grant.Scopes, &rec.grant.Resource, &rec.grant.RevokedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return errInvalid
		}
		if err != nil {
			return err
		}
		// A different client or redirect must not consume the code or revoke
		// an existing grant by presenting a code that does not belong to it.
		if rec.grant.ClientID != clientID || rec.redirectURI != redirectURI {
			return errInvalid
		}
		if used != nil {
			reused = true
			_, err := tx.Exec(ctx, `UPDATE oauth_grant SET revoked_at = coalesce(revoked_at, now()) WHERE id = $1`, rec.grant.ID)
			if err == nil {
				_, err = tx.Exec(ctx, `UPDATE oauth_token SET revoked_at = now() WHERE grant_id = $1 AND revoked_at IS NULL`, rec.grant.ID)
			}
			return err
		}
		if rec.grant.RevokedAt != nil || s.now().After(expires) {
			return errInvalid
		}
		_, err = tx.Exec(ctx, `UPDATE oauth_code SET used_at = now() WHERE code_hash = $1`, hashToken(code))
		return err
	})
	if err != nil {
		return codeRecord{}, err
	}
	if reused {
		return codeRecord{}, errReuse
	}
	return rec, nil
}

// ---- tokens ----

// Tokens is an issued token pair.
type Tokens struct {
	Access    string
	Refresh   string
	ExpiresIn int
	Scopes    []string
}

// issue creates an access and a refresh token for g. parent is the refresh
// token being rotated, if any. q is a transaction or the pool.
func (s *Store) issue(ctx context.Context, q execer, g Grant, parent []byte) (Tokens, error) {
	access, ah, err := newToken(prefixAccess)
	if err != nil {
		return Tokens{}, err
	}
	refresh, rh, err := newToken(prefixRefresh)
	if err != nil {
		return Tokens{}, err
	}
	now := s.now()
	if _, err := q.Exec(ctx, `INSERT INTO oauth_token (token_hash, kind, grant_id, scopes, expires_at) VALUES ($1, 'access', $2, $3, $4)`,
		ah, g.ID, g.Scopes, now.Add(accessTTL)); err != nil {
		return Tokens{}, err
	}
	if _, err := q.Exec(ctx, `INSERT INTO oauth_token (token_hash, kind, grant_id, scopes, parent, expires_at) VALUES ($1, 'refresh', $2, $3, $4, $5)`,
		rh, g.ID, g.Scopes, parent, now.Add(refreshTTL)); err != nil {
		return Tokens{}, err
	}
	return Tokens{Access: access, Refresh: refresh, ExpiresIn: int(accessTTL.Seconds()), Scopes: g.Scopes}, nil
}

type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// refresh rotates a refresh token. Presenting a refresh token that was
// already rotated revokes the grant: either the client or an attacker has a
// stolen copy, and the owner has to approve the client again.
func (s *Store) refresh(ctx context.Context, clientID, token, scope string) (Tokens, error) {
	var out Tokens
	reused := false
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var g Grant
		var expires time.Time
		var revoked *time.Time
		h := hashToken(token)
		err := tx.QueryRow(ctx, `
			SELECT t.expires_at, t.revoked_at, g.id, g.client_id, g.agent, t.scopes, g.resource, g.revoked_at
			FROM oauth_token t JOIN oauth_grant g ON g.id = t.grant_id
			WHERE t.token_hash = $1 AND t.kind = 'refresh' FOR UPDATE OF t, g`, h).
			Scan(&expires, &revoked, &g.ID, &g.ClientID, &g.Agent, &g.Scopes, &g.Resource, &g.RevokedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return errInvalid
		}
		if err != nil {
			return err
		}
		if g.ClientID != clientID || g.RevokedAt != nil || s.now().After(expires) {
			return errInvalid
		}
		if revoked != nil {
			// Rotated before: revoke everything under this grant.
			reused = true
			if _, err := tx.Exec(ctx, `UPDATE oauth_grant SET revoked_at = now() WHERE id = $1`, g.ID); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `UPDATE oauth_token SET revoked_at = now() WHERE grant_id = $1 AND revoked_at IS NULL`, g.ID)
			return err
		}
		if scope != "" {
			// Validate before rotation: an invalid request must leave the old
			// refresh token usable. Bind the new pair to the requested subset,
			// so later refreshes cannot restore scopes previously relinquished.
			if !subset(strings.Fields(scope), g.Scopes) {
				return errInvalidScope
			}
			g.Scopes = authz.Normalize(scope)
			if len(g.Scopes) == 0 {
				return errInvalidScope
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE oauth_token SET revoked_at = now() WHERE token_hash = $1`, h); err != nil {
			return err
		}
		out, err = s.issue(ctx, tx, g, h)
		return err
	})
	if err != nil {
		return Tokens{}, err
	}
	if reused {
		return Tokens{}, errReuse
	}
	return out, nil
}

// revokeToken revokes a token presented by its client (RFC 7009). Revoking a
// refresh token also revokes the access tokens of its grant. Unknown tokens
// are not an error.
func (s *Store) revokeToken(ctx context.Context, clientID, token string) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var kind, grantID string
		err := tx.QueryRow(ctx, `
			SELECT t.kind, t.grant_id::text FROM oauth_token t JOIN oauth_grant g ON g.id = t.grant_id
			WHERE t.token_hash = $1 AND g.client_id = $2 FOR UPDATE OF t, g`, hashToken(token), clientID).Scan(&kind, &grantID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if kind == "refresh" {
			// Serialize with refresh rotation, and revoke the grant so token
			// issuance racing this request cannot restore access afterward.
			if _, err := tx.Exec(ctx, `UPDATE oauth_grant SET revoked_at = coalesce(revoked_at, now()) WHERE id = $1`, grantID); err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `UPDATE oauth_token SET revoked_at = now() WHERE grant_id = $1 AND revoked_at IS NULL`, grantID)
		} else {
			_, err = tx.Exec(ctx, `UPDATE oauth_token SET revoked_at = now() WHERE token_hash = $1 AND revoked_at IS NULL`, hashToken(token))
		}
		return err
	})
}

// AccessToken is a verified access token.
type AccessToken struct {
	GrantID  string
	ClientID string
	Agent    string
	Scopes   []string
	Resource string
	Expires  time.Time
}

// verifyAccess looks up a live access token and records the grant's use.
func (s *Store) verifyAccess(ctx context.Context, token string) (AccessToken, error) {
	var a AccessToken
	err := s.pool.QueryRow(ctx, `
		SELECT g.id, g.client_id, g.agent, t.scopes, g.resource, t.expires_at
		FROM oauth_token t JOIN oauth_grant g ON g.id = t.grant_id
		WHERE t.token_hash = $1 AND t.kind = 'access' AND t.revoked_at IS NULL AND g.revoked_at IS NULL AND t.expires_at > $2`,
		hashToken(token), s.now()).Scan(&a.GrantID, &a.ClientID, &a.Agent, &a.Scopes, &a.Resource, &a.Expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return AccessToken{}, errInvalid
	}
	if err != nil {
		return AccessToken{}, err
	}
	// Coarse, to avoid a write per request.
	_, _ = s.pool.Exec(ctx, `UPDATE oauth_grant SET last_used_at = now() WHERE id = $1 AND (last_used_at IS NULL OR last_used_at < now() - interval '1 minute')`, a.GrantID)
	return a, nil
}

// Prune deletes expired codes and tokens, and clients that never got a grant
// (abandoned registrations) after a day. It returns the rows deleted.
func (s *Store) Prune(ctx context.Context) (int64, error) {
	var total int64
	for _, q := range []string{
		`DELETE FROM oauth_code WHERE expires_at < now() - interval '1 day'`,
		`DELETE FROM oauth_token WHERE expires_at < now() - interval '1 day'`,
		`DELETE FROM oauth_client c WHERE created_at < now() - interval '1 day'
		   AND NOT EXISTS (SELECT 1 FROM oauth_grant g WHERE g.client_id = c.client_id)`,
	} {
		tag, err := s.pool.Exec(ctx, q)
		if err != nil {
			return total, err
		}
		total += tag.RowsAffected()
	}
	return total, nil
}
