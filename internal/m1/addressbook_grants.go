package m1

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
)

type addressGrant struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	Permission int16  `json:"permission"`
}

type grantPage struct {
	Shares []addressGrant `json:"shares"`
	Next   string         `json:"next,omitempty"`
}

func (a *API) listBookGrants(w http.ResponseWriter, r *http.Request) {
	book, ok := bookPathID(w, r, "book", errBookMissing)
	if !ok {
		return
	}
	after, ok := bookCursor(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	out := grantPage{Shares: make([]addressGrant, 0)}
	err := a.bookActor(r, false, func(tx pgx.Tx, who actor) error {
		if _, err := bookFor(ctx, tx, who, book, bookManage); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT g.id,CASE WHEN g.user_id IS NOT NULL THEN 'user' ELSE 'group' END,
			COALESCE(u.username,ug.name),g.permission FROM address_book_grants g
			LEFT JOIN users u ON u.id=g.user_id LEFT JOIN user_groups ug ON ug.id=g.group_id
			WHERE g.book_id=$1 AND ($2='' OR g.id>NULLIF($2,'')::uuid) ORDER BY g.id LIMIT 101`, book, after)
		if err != nil {
			return err
		}
		out.Shares, err = pgx.AppendRows(out.Shares, rows, pgx.RowToStructByPos[addressGrant])
		return err
	})
	if err != nil {
		failure(w, "list address book shares", err)
		return
	}
	if len(out.Shares) > 100 {
		out.Shares = out.Shares[:100]
		out.Next = out.Shares[99].ID
	}
	writeJSON(w, 200, out)
}

func (a *API) grantBook(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.sessionWrite(w, r); !ok {
		return
	}
	book, ok := bookPathID(w, r, "book", errBookMissing)
	if !ok {
		return
	}
	var in struct {
		Kind       string `json:"kind"`
		Name       string `json:"name"`
		Permission int16  `json:"permission"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if !validOrganization(in.Name) || (in.Kind != "user" && in.Kind != "group") || in.Permission < bookView || in.Permission > bookManage {
		writeError(w, 400, "invalid_address_book_share")
		return
	}
	ctx := r.Context()
	err := a.bookActor(r, true, func(tx pgx.Tx, who actor) error {
		if _, err := bookFor(ctx, tx, who, book, bookManage); err != nil {
			return err
		}
		var user, group string
		switch in.Kind {
		case "user":
			id, err := userByUsername(ctx, tx, who.Organization, normalizeUsername(in.Name))
			if err != nil {
				return err
			}
			var owner bool
			if err := tx.QueryRow(ctx, "SELECT owner_id=$2 FROM address_books WHERE id=$1", book, id).Scan(&owner); err != nil {
				return err
			}
			if owner {
				return errBookShareOwner
			}
			user = id
		case "group":
			err := tx.QueryRow(ctx, "SELECT id FROM user_groups WHERE organization_id=$1 AND name=$2", who.Organization, in.Name).Scan(&group)
			if errors.Is(err, pgx.ErrNoRows) {
				return errGroupMissing
			}
			if err != nil {
				return err
			}
		}
		var grantID string
		err := tx.QueryRow(ctx, `INSERT INTO address_book_grants AS g(organization_id,book_id,user_id,group_id,permission)
			VALUES($1,$2,NULLIF($3,'')::uuid,NULLIF($4,'')::uuid,$5)
			ON CONFLICT(book_id,user_id,group_id) DO UPDATE SET permission=EXCLUDED.permission
			WHERE g.permission<>EXCLUDED.permission RETURNING id`, who.Organization, book, user, group, in.Permission).Scan(&grantID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		return auditWithReason(ctx, tx, who.Organization, who.ID, "address_book.shared", book,
			fmt.Sprintf("%s %s %s %d", grantID, in.Kind, user+group, in.Permission))
	})
	if err != nil {
		failure(w, "share address book", err)
		return
	}
	w.WriteHeader(204)
}

func (a *API) revokeBook(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.sessionWrite(w, r); !ok {
		return
	}
	book, ok := bookPathID(w, r, "book", errBookMissing)
	if !ok {
		return
	}
	grant, ok := bookPathID(w, r, "grant", errGrantMissing)
	if !ok {
		return
	}
	ctx := r.Context()
	err := a.bookActor(r, true, func(tx pgx.Tx, who actor) error {
		if _, err := bookFor(ctx, tx, who, book, bookManage); err != nil {
			return err
		}
		var kind, subject string
		var permission int16
		err := tx.QueryRow(ctx, `DELETE FROM address_book_grants WHERE book_id=$1 AND id=$2
			RETURNING CASE WHEN user_id IS NOT NULL THEN 'user' ELSE 'group' END,
			COALESCE(user_id,group_id),permission`, book, grant).Scan(&kind, &subject, &permission)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		return auditWithReason(ctx, tx, who.Organization, who.ID, "address_book.share_revoked", book,
			fmt.Sprintf("%s %s %s %d", grant, kind, subject, permission))
	})
	if err != nil {
		failure(w, "revoke address book share", err)
		return
	}
	w.WriteHeader(204)
}
