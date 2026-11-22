package m1

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"embed"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
	"golang.org/x/crypto/argon2"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

const (
	adminRole       = "platform_admin"
	ticketLifetime  = 15 * time.Minute
	sessionLifetime = 12 * time.Hour
)

var (
	errInitialized  = errors.New("already_initialized")
	errTicket       = errors.New("invalid_or_expired_ticket")
	errCredentials  = errors.New("invalid_credentials")
	errSession      = errors.New("session_required")
	usernamePattern = regexp.MustCompile(`^[a-z][a-z0-9._-]{2,39}$`)
)

type Store struct {
	DB *pgxpool.Pool
}

func Migrate(ctx context.Context, dsn string) error {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	files, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		return err
	}
	return migrateFrom(ctx, db, files)
}

func migrateFrom(ctx context.Context, db *sql.DB, files fs.FS) error {
	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		return err
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, db, files, goose.WithSessionLocker(locker))
	if err != nil {
		return err
	}
	_, err = provider.Up(ctx)
	return err
}

func token() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

func digest(value string) []byte {
	h := sha256.Sum256([]byte(value))
	return h[:]
}

func validToken(value string) bool {
	if len(value) != 43 || strings.ContainsAny(value, "\r\n") {
		return false
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(value)
	return err == nil && len(raw) == 32
}

func (s Store) Initialized(ctx context.Context) (bool, error) {
	var initialized bool
	err := s.DB.QueryRow(ctx, `SELECT initialized_at IS NOT NULL FROM bootstrap_state WHERE singleton`).Scan(&initialized)
	return initialized, err
}

func (s Store) IssueTicket(ctx context.Context) (string, error) {
	value, err := token()
	if err != nil {
		return "", err
	}
	// UPDATE locks the same singleton as the redeem transaction.
	tag, err := s.DB.Exec(ctx, `
		UPDATE bootstrap_state SET ticket_digest = $1, ticket_expires_at = now() + $2::interval
		WHERE singleton AND initialized_at IS NULL`, digest(value), fmt.Sprintf("%d seconds", int(ticketLifetime.Seconds())))
	if err != nil {
		return "", err
	}
	if tag.RowsAffected() != 1 {
		return "", errInitialized
	}
	return value, nil
}

type bootstrapRequest struct {
	Organization string `json:"organization"`
	Username     string `json:"username"`
	Password     string `json:"password"`
	Ticket       string `json:"ticket"`
}

func (s Store) initialize(ctx context.Context, in bootstrapRequest) error {
	hash, err := hashPassword(in.Password)
	if err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		var initialized, valid bool
		err := tx.QueryRow(ctx, `
			SELECT initialized_at IS NOT NULL,
			       COALESCE(ticket_digest = $1 AND ticket_expires_at > clock_timestamp(), false)
			FROM bootstrap_state WHERE singleton FOR UPDATE`, digest(in.Ticket)).Scan(&initialized, &valid)
		if err != nil {
			return err
		}
		if initialized {
			return errInitialized
		}
		if !valid {
			return errTicket
		}
		var orgID, userID, roleID string
		if err := tx.QueryRow(ctx, "INSERT INTO organizations (name) VALUES ($1) RETURNING id", in.Organization).Scan(&orgID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `INSERT INTO users (organization_id, username, password_hash)
			VALUES ($1, $2, $3) RETURNING id`, orgID, in.Username, hash).Scan(&userID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `INSERT INTO roles (organization_id, name) VALUES ($1, $2) RETURNING id`, orgID, adminRole).Scan(&roleID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO role_bindings (organization_id, user_id, role_id) VALUES ($1, $2, $3)`, orgID, userID, roleID); err != nil {
			return err
		}
		if err := audit(ctx, tx, orgID, userID, "organization.initialized", orgID); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE bootstrap_state SET initialized_at = now(), ticket_digest = NULL,
			ticket_expires_at = NULL WHERE singleton`)
		return err
	})
}

func audit(ctx context.Context, tx pgx.Tx, organizationID, actorID, action, resourceID string) error {
	_, err := tx.Exec(ctx, `
		WITH event AS (
			INSERT INTO audit_events (organization_id, actor_id, action, result, resource_id)
			VALUES ($1, $2, $3, 'success', $4) RETURNING id
		)
		INSERT INTO outbox (event_id) SELECT id FROM event`, organizationID, actorID, action, resourceID)
	return err
}

type principal struct {
	UserID       string `json:"user_id"`
	Username     string `json:"username"`
	Organization string `json:"organization"`
	Role         string `json:"role"`
	CSRF         string `json:"csrf_token,omitempty"`
}

func (s Store) login(ctx context.Context, username, password, previous string) (string, error) {
	var userID, orgID, hash string
	err := s.DB.QueryRow(ctx, `SELECT id, organization_id, COALESCE(password_hash, '') FROM users
		WHERE username = $1 AND status = 'active'`, username).Scan(&userID, &orgID, &hash)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	if !passwordMatches(password, hash) || userID == "" {
		return "", errCredentials
	}
	value, err := token()
	if err != nil {
		return "", err
	}
	err = pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		var currentHash string
		err := tx.QueryRow(ctx, `SELECT COALESCE(password_hash, '') FROM users
			WHERE id = $1 AND status = 'active' FOR SHARE`, userID).Scan(&currentHash)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && currentHash != hash) {
			return errCredentials
		}
		if err != nil {
			return err
		}
		if validToken(previous) {
			if _, err := tx.Exec(ctx, `UPDATE browser_sessions SET revoked_at = now()
				WHERE token_digest = $1 AND revoked_at IS NULL`, digest(previous)); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `INSERT INTO browser_sessions (token_digest, user_id, expires_at)
			VALUES ($1, $2, now() + $3::interval)`, digest(value), userID, fmt.Sprintf("%d seconds", int(sessionLifetime.Seconds()))); err != nil {
			return err
		}
		return audit(ctx, tx, orgID, userID, "user.login", userID)
	})
	return value, err
}

func (s Store) session(ctx context.Context, value string) (principal, error) {
	var user principal
	if !validToken(value) {
		return user, errSession
	}
	err := s.DB.QueryRow(ctx, `
		SELECT u.id, u.username, o.name, COALESCE((
			SELECT r.name FROM role_bindings b JOIN roles r ON r.id = b.role_id
			WHERE b.user_id = u.id ORDER BY (r.name = 'platform_admin') DESC, r.name LIMIT 1
		), '')
		FROM browser_sessions s JOIN users u ON u.id = s.user_id
		JOIN organizations o ON o.id = u.organization_id
		WHERE s.token_digest = $1 AND s.revoked_at IS NULL
		  AND s.expires_at > now() AND u.status = 'active'`, digest(value)).
		Scan(&user.UserID, &user.Username, &user.Organization, &user.Role)
	if errors.Is(err, pgx.ErrNoRows) {
		return user, errSession
	}
	if err == nil {
		user.CSRF = base64.RawURLEncoding.EncodeToString(digest("csrf\x00" + value))
	}
	return user, err
}

func (s Store) logout(ctx context.Context, value string, user principal) error {
	return pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		var orgID string
		err := tx.QueryRow(ctx, `UPDATE browser_sessions s SET revoked_at = now()
			FROM users u WHERE s.token_digest = $1 AND s.revoked_at IS NULL AND s.user_id = u.id
			RETURNING u.organization_id`, digest(value)).Scan(&orgID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		return audit(ctx, tx, orgID, user.UserID, "user.logout", user.UserID)
	})
}

const (
	hashPrefix  = "$argon2id$v=19$m=19456,t=2,p=1$"
	argonMemory = 19 * 1024
)

func hashPassword(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	hash := argon2.IDKey([]byte(password), salt, 2, argonMemory, 1, 32)
	return hashPrefix + base64.RawStdEncoding.EncodeToString(salt) + "$" + base64.RawStdEncoding.EncodeToString(hash), nil
}

func passwordMatches(password, encoded string) bool {
	// An absent account takes the same KDF path as a real account.
	salt, expected := make([]byte, 16), make([]byte, 32)
	valid := false
	if content, ok := strings.CutPrefix(encoded, hashPrefix); ok {
		left, right, separated := strings.Cut(content, "$")
		s, se := base64.RawStdEncoding.Strict().DecodeString(left)
		h, he := base64.RawStdEncoding.Strict().DecodeString(right)
		if separated && se == nil && he == nil && len(s) == len(salt) && len(h) == len(expected) {
			salt, expected, valid = s, h, true
		}
	}
	actual := argon2.IDKey([]byte(password), salt, 2, argonMemory, 1, 32)
	return subtle.ConstantTimeCompare(actual, expected) == 1 && valid
}

func normalizeUsername(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func validOrganization(value string) bool {
	return value != "" && len([]rune(value)) <= 80 && strings.IndexFunc(value, unicode.IsControl) < 0
}
