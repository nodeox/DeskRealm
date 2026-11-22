package m1

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
)

const (
	bookView int16 = iota + 1
	bookEdit
	bookManage
)

var (
	errBookMissing    = errors.New("address_book_not_found")
	errBookConflict   = errors.New("address_book_name_in_use")
	errEntryMissing   = errors.New("address_book_entry_not_found")
	errEntryConflict  = errors.New("address_book_device_exists")
	errEntryStale     = errors.New("address_book_entry_stale")
	errEntryInactive  = errors.New("address_book_device_inactive")
	errGrantMissing   = errors.New("address_book_share_not_found")
	errBookShareOwner = errors.New("address_book_owner_share")
)

// Platform management rights alone do not grant access to someone's book.
const bookAccess = `CASE WHEN b.owner_id=$2 THEN 3::smallint ELSE COALESCE((
	SELECT max(a.permission) FROM address_book_grants a WHERE a.book_id=b.id AND (
		a.user_id=$2 OR EXISTS (SELECT 1 FROM user_group_members m WHERE m.group_id=a.group_id AND m.user_id=$2)
	)
),0::smallint) END`

type addressBook struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Owner      bool   `json:"owner"`
	Permission int16  `json:"permission"`
}

type bookPage struct {
	Books []addressBook `json:"books"`
	Next  string        `json:"next,omitempty"`
}

func bookFor(ctx context.Context, tx pgx.Tx, who actor, id string, need int16) (addressBook, error) {
	var b addressBook
	err := tx.QueryRow(ctx, `SELECT b.id,b.name,b.owner_id=$2,`+bookAccess+` FROM address_books b
		WHERE b.organization_id=$1 AND b.id=$3`, who.Organization, who.ID, id).Scan(&b.ID, &b.Name, &b.Owner, &b.Permission)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && b.Permission == 0) {
		return b, errBookMissing
	}
	if err == nil && b.Permission < need {
		return b, errForbidden
	}
	return b, err
}

func (a *API) bookActor(r *http.Request, write bool, run func(pgx.Tx, actor) error) error {
	ctx := r.Context()
	options := pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}
	if write {
		options = pgx.TxOptions{}
	}
	return pgx.BeginTxFunc(ctx, a.store.DB, options, func(tx pgx.Tx) error {
		who, err := actorSession(ctx, tx, a.cookieValue(r), write)
		if err != nil {
			return err
		}
		return run(tx, who)
	})
}

func bookPathID(w http.ResponseWriter, r *http.Request, name string, missing error) (string, bool) {
	id := r.PathValue(name)
	if !userIDPattern.MatchString(id) {
		failure(w, "read address book resource", missing)
		return "", false
	}
	return id, true
}

func bookCursor(w http.ResponseWriter, r *http.Request) (string, bool) {
	after := r.URL.Query().Get("after")
	if after != "" && !userIDPattern.MatchString(after) {
		writeError(w, 400, "invalid_cursor")
		return "", false
	}
	return after, true
}

func (a *API) listBooks(w http.ResponseWriter, r *http.Request) {
	after, ok := bookCursor(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	out := bookPage{Books: make([]addressBook, 0)}
	err := a.bookActor(r, false, func(tx pgx.Tx, who actor) error {
		rows, err := tx.Query(ctx, `SELECT b.id,b.name,b.owner_id=$2,`+bookAccess+` FROM address_books b
			WHERE b.organization_id=$1 AND `+bookAccess+`>0
			AND ($3='' OR b.id>NULLIF($3,'')::uuid) ORDER BY b.id LIMIT 101`, who.Organization, who.ID, after)
		if err != nil {
			return err
		}
		out.Books, err = pgx.AppendRows(out.Books, rows, pgx.RowToStructByPos[addressBook])
		return err
	})
	if err != nil {
		failure(w, "list address books", err)
		return
	}
	if len(out.Books) > 100 {
		out.Books = out.Books[:100]
		out.Next = out.Books[99].ID
	}
	writeJSON(w, 200, out)
}

func (a *API) readBook(w http.ResponseWriter, r *http.Request) {
	id, ok := bookPathID(w, r, "book", errBookMissing)
	if !ok {
		return
	}
	var out addressBook
	ctx := r.Context()
	err := a.bookActor(r, false, func(tx pgx.Tx, who actor) error {
		var err error
		out, err = bookFor(ctx, tx, who, id, bookView)
		return err
	})
	if err != nil {
		failure(w, "read address book", err)
		return
	}
	writeJSON(w, 200, out)
}

func (a *API) saveBook(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.sessionWrite(w, r); !ok {
		return
	}
	id := r.PathValue("book")
	if id != "" && !userIDPattern.MatchString(id) {
		failure(w, "save address book", errBookMissing)
		return
	}
	var in struct {
		Name string `json:"name"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if !validOrganization(in.Name) {
		writeError(w, 400, "invalid_address_book")
		return
	}
	ctx := r.Context()
	out := addressBook{Name: in.Name, Owner: true, Permission: bookManage}
	err := a.bookActor(r, true, func(tx pgx.Tx, who actor) error {
		event := "address_book.created"
		var err error
		if id == "" {
			err = tx.QueryRow(ctx, `INSERT INTO address_books(organization_id,owner_id,name)
				VALUES($1,$2,$3) RETURNING id`, who.Organization, who.ID, in.Name).Scan(&out.ID)
		} else {
			out, err = bookFor(ctx, tx, who, id, bookManage)
			if err != nil || out.Name == in.Name {
				return err
			}
			out.Name = in.Name
			event = "address_book.renamed"
			_, err = tx.Exec(ctx, "UPDATE address_books SET name=$2 WHERE id=$1", id, in.Name)
		}
		if uniqueViolation(err) {
			return errBookConflict
		}
		if err != nil {
			return err
		}
		return audit(ctx, tx, who.Organization, who.ID, event, out.ID)
	})
	if err != nil {
		failure(w, "save address book", err)
		return
	}
	status := http.StatusOK
	if id == "" {
		status = http.StatusCreated
	}
	writeJSON(w, status, out)
}
