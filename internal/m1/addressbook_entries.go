package m1

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

type addressEntry struct {
	ID         string   `json:"id"`
	DeviceID   string   `json:"device_id"`
	RustDeskID string   `json:"rustdesk_id"`
	State      string   `json:"registration_state"`
	Alias      string   `json:"alias"`
	Tags       []string `json:"tags"`
	Note       string   `json:"note"`
	Version    int64    `json:"version"`
}

type entryPage struct {
	Entries []addressEntry `json:"entries"`
	Next    string         `json:"next,omitempty"`
}

const addressEntryProjection = `e.id,e.device_id,d.rustdesk_id,d.registration_state,e.alias,e.tags,e.note,e.version`
const visibleAddressEntries = `FROM address_book_entries e JOIN devices d ON d.id=e.device_id
	JOIN device_groups dg ON dg.id=d.group_id WHERE e.organization_id=$1 AND ` + deviceGroupRead + ` AND e.book_id=$4`

func addressEntryFor(ctx context.Context, tx pgx.Tx, who actor, book, id string) (addressEntry, error) {
	rows, err := tx.Query(ctx, `SELECT `+addressEntryProjection+` `+visibleAddressEntries+` AND e.id=$5`,
		who.Organization, who.ID, who.PlatformAdmin, book, id)
	if err != nil {
		return addressEntry{}, err
	}
	out, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByPos[addressEntry])
	if errors.Is(err, pgx.ErrNoRows) {
		err = errEntryMissing
	}
	return out, err
}

// ResolveAddressBookDevice verifies current book access, device visibility and
// registration state. Session admission still needs to apply the connection ACL.
func (s Store) ResolveAddressBookDevice(ctx context.Context, session, bookID, entryID string) (string, error) {
	var deviceID string
	if !userIDPattern.MatchString(bookID) || !userIDPattern.MatchString(entryID) {
		return "", errEntryMissing
	}
	err := pgx.BeginTxFunc(ctx, s.DB, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		who, err := actorSession(ctx, tx, session, false)
		if err != nil {
			return err
		}
		deviceID, err = resolveAddressBookDevice(ctx, tx, who, bookID, entryID)
		return err
	})
	return deviceID, err
}

// resolveAddressBookDevice can run inside the same transaction as admission.
func resolveAddressBookDevice(ctx context.Context, tx pgx.Tx, who actor, bookID, entryID string) (string, error) {
	if _, err := bookFor(ctx, tx, who, bookID, bookView); err != nil {
		return "", err
	}
	entry, err := addressEntryFor(ctx, tx, who, bookID, entryID)
	if err != nil {
		return "", err
	}
	if entry.State != "approved" {
		return "", errEntryInactive
	}
	return entry.DeviceID, nil
}

func (a *API) readBookEntries(w http.ResponseWriter, r *http.Request) {
	book, ok := bookPathID(w, r, "book", errBookMissing)
	if !ok {
		return
	}
	entry := r.PathValue("entry")
	if entry != "" && !userIDPattern.MatchString(entry) {
		failure(w, "read address book entry", errEntryMissing)
		return
	}
	after, ok := bookCursor(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	out := entryPage{Entries: make([]addressEntry, 0)}
	err := a.bookActor(r, false, func(tx pgx.Tx, who actor) error {
		if _, err := bookFor(ctx, tx, who, book, bookView); err != nil {
			return err
		}
		if entry != "" {
			result, err := addressEntryFor(ctx, tx, who, book, entry)
			if err == nil {
				out.Entries = append(out.Entries, result)
			}
			return err
		}
		rows, err := tx.Query(ctx, `SELECT `+addressEntryProjection+` `+visibleAddressEntries+`
			AND ($5='' OR e.id>NULLIF($5,'')::uuid) ORDER BY e.id LIMIT 101`, who.Organization, who.ID, who.PlatformAdmin, book, after)
		if err != nil {
			return err
		}
		out.Entries, err = pgx.AppendRows(out.Entries, rows, pgx.RowToStructByPos[addressEntry])
		return err
	})
	if err != nil {
		failure(w, "read address book entries", err)
		return
	}
	if entry != "" {
		writeJSON(w, 200, out.Entries[0])
		return
	}
	if len(out.Entries) > 100 {
		out.Entries = out.Entries[:100]
		out.Next = out.Entries[99].ID
	}
	writeJSON(w, 200, out)
}

type entryInput struct {
	DeviceID string   `json:"device_id"`
	Alias    string   `json:"alias"`
	Tags     []string `json:"tags"`
	Note     string   `json:"note"`
	Version  int64    `json:"version"`
}

func validEntryText(value string, max int, allowLines bool) bool {
	if utf8.RuneCountInString(value) > max {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) && !(allowLines && (r == '\n' || r == '\t')) {
			return false
		}
	}
	return true
}

func (in *entryInput) validate() bool {
	in.Alias = strings.TrimSpace(in.Alias)
	in.Note = strings.TrimSpace(strings.ReplaceAll(in.Note, "\r\n", "\n"))
	if !validEntryText(in.Alias, 80, false) || !validEntryText(in.Note, 500, true) || len(in.Tags) > 12 {
		return false
	}
	if in.Tags == nil {
		in.Tags = []string{}
	}
	seen := make(map[string]bool, len(in.Tags))
	for i, raw := range in.Tags {
		tag := strings.TrimSpace(raw)
		if tag == "" || !validEntryText(tag, 32, false) || seen[tag] {
			return false
		}
		in.Tags[i] = tag
		seen[tag] = true
	}
	return true
}

func (a *API) saveEntry(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.sessionWrite(w, r); !ok {
		return
	}
	book, ok := bookPathID(w, r, "book", errBookMissing)
	if !ok {
		return
	}
	entry := r.PathValue("entry")
	if entry != "" && !userIDPattern.MatchString(entry) {
		failure(w, "save address book entry", errEntryMissing)
		return
	}
	var in entryInput
	if !readJSON(w, r, &in) {
		return
	}
	validTarget := (entry == "" && in.Version == 0 && userIDPattern.MatchString(in.DeviceID)) ||
		(entry != "" && in.Version > 0 && in.DeviceID == "")
	if !validTarget || !in.validate() {
		writeError(w, 400, "invalid_address_book_entry")
		return
	}
	ctx := r.Context()
	var out addressEntry
	err := a.bookActor(r, true, func(tx pgx.Tx, who actor) error {
		if _, err := bookFor(ctx, tx, who, book, bookEdit); err != nil {
			return err
		}
		event := "address_book.entry_added"
		var err error
		if entry == "" {
			var visible bool
			err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM devices d JOIN device_groups dg ON dg.id=d.group_id
				WHERE d.organization_id=$1 AND `+deviceGroupRead+` AND d.id=$4)`,
				who.Organization, who.ID, who.PlatformAdmin, in.DeviceID).Scan(&visible)
			if err != nil {
				return err
			}
			if !visible {
				return errDeviceMissing
			}
			err = tx.QueryRow(ctx, `INSERT INTO address_book_entries(organization_id,book_id,device_id,alias,tags,note)
				VALUES($1,$2,$3,$4,$5,$6) RETURNING id`, who.Organization, book, in.DeviceID, in.Alias, in.Tags, in.Note).Scan(&entry)
		} else {
			event = "address_book.entry_edited"
			prior, err := addressEntryFor(ctx, tx, who, book, entry)
			if err != nil {
				return err
			}
			if prior.Version != in.Version {
				return errEntryStale
			}
			if _, err = tx.Exec(ctx, `UPDATE address_book_entries SET alias=$2,tags=$3,note=$4,version=version+1
				WHERE id=$1`, entry, in.Alias, in.Tags, in.Note); err != nil {
				return err
			}
		}
		if uniqueViolation(err) {
			return errEntryConflict
		}
		if err != nil {
			return err
		}
		out, err = addressEntryFor(ctx, tx, who, book, entry)
		if err != nil {
			return err
		}
		return auditWithReason(ctx, tx, who.Organization, who.ID, event, book, entry)
	})
	if err != nil {
		failure(w, "save address book entry", err)
		return
	}
	status := http.StatusOK
	if in.DeviceID != "" {
		status = http.StatusCreated
	}
	writeJSON(w, status, out)
}

func (a *API) removeEntry(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.sessionWrite(w, r); !ok {
		return
	}
	book, ok := bookPathID(w, r, "book", errBookMissing)
	if !ok {
		return
	}
	entry, ok := bookPathID(w, r, "entry", errEntryMissing)
	if !ok {
		return
	}
	var in struct {
		Version int64 `json:"version"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if in.Version < 1 {
		writeError(w, 400, "invalid_address_book_entry")
		return
	}
	ctx := r.Context()
	err := a.bookActor(r, true, func(tx pgx.Tx, who actor) error {
		if _, err := bookFor(ctx, tx, who, book, bookEdit); err != nil {
			return err
		}
		prior, err := addressEntryFor(ctx, tx, who, book, entry)
		if err != nil {
			return err
		}
		if prior.Version != in.Version {
			return errEntryStale
		}
		if _, err := tx.Exec(ctx, "DELETE FROM address_book_entries WHERE id=$1", entry); err != nil {
			return err
		}
		if err := recheckSessions(ctx, tx, "book_id=$1 AND entry_id=$2", book, entry); err != nil {
			return err
		}
		return auditWithReason(ctx, tx, who.Organization, who.ID, "address_book.entry_removed", book, entry)
	})
	if err != nil {
		failure(w, "remove address book entry", err)
		return
	}
	w.WriteHeader(204)
}
