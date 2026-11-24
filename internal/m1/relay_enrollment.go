package m1

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"time"

	"deskrealm/internal/relayprotocol"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const relayEnrollmentPrefix = "dre_"
const relayTimestampWindow = 30 * time.Second

var relayLabel = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
var relayBuild = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$`)
var relayDNSLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

type relayRequest struct {
	Name     string `json:"name"`
	Site     string `json:"site"`
	Region   string `json:"region"`
	Endpoint string `json:"endpoint"`
	Build    string `json:"build"`
	Capacity int    `json:"capacity"`
}

func normalizedRelayEndpoint(value string) string {
	host, rawPort, err := net.SplitHostPort(value)
	if err != nil {
		return ""
	}
	port, err := strconv.ParseUint(rawPort, 10, 16)
	if err != nil || port == 0 {
		return ""
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		if ip.Zone() != "" {
			return ""
		}
		ip = ip.Unmap()
		if ip.IsUnspecified() || ip.IsMulticast() {
			return ""
		}
		host = ip.String()
	} else {
		host = strings.TrimSuffix(strings.ToLower(host), ".")
		if len(host) == 0 || len(host) > 253 {
			return ""
		}
		for _, label := range strings.Split(host, ".") {
			if !relayDNSLabel.MatchString(label) {
				return ""
			}
		}
	}
	return net.JoinHostPort(host, strconv.FormatUint(port, 10))
}

type relayCredential struct {
	NodeID     string    `json:"node_id"`
	TaskID     string    `json:"task_id"`
	Credential string    `json:"credential"`
	Expires    time.Time `json:"expires_at"`
}

func (a *API) issueRelayEnrollment(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.sessionWrite(w, r); !ok {
		return
	}
	var input relayRequest
	node := r.PathValue("id")
	if node != "" {
		if !userIDPattern.MatchString(node) {
			failure(w, "issue relay enrollment", errTaskMissing)
			return
		}
	} else {
		if !readJSON(w, r, &input) {
			return
		}
		input.Endpoint = normalizedRelayEndpoint(input.Endpoint)
		if !relayLabel.MatchString(input.Name) || !relayLabel.MatchString(input.Site) ||
			!relayLabel.MatchString(input.Region) || !relayBuild.MatchString(input.Build) ||
			input.Endpoint == "" || input.Capacity < 1 || input.Capacity > 100000 {
			failure(w, "issue relay enrollment", errTaskInvalid)
			return
		}
	}
	ctx := r.Context()
	out := relayCredential{NodeID: node}
	err := a.withActor(r, true, func(tx pgx.Tx, who actor) error {
		if !who.PlatformAdmin {
			return errForbidden
		}
		raw, err := token()
		if err != nil {
			return err
		}
		out.Credential = relayEnrollmentPrefix + raw
		if node == "" {
			if err := tx.QueryRow(ctx, `INSERT INTO tasks(organization_id,kind,owner_id)
				VALUES($1,'relay_enrollment',$2) RETURNING id`, who.Organization, who.ID).Scan(&out.TaskID); err != nil {
				return err
			}
			err := tx.QueryRow(ctx, `INSERT INTO relay_nodes(organization_id,task_id,name,site,region,endpoint,build,capacity)
				VALUES($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT (endpoint) DO NOTHING RETURNING id`,
				who.Organization, out.TaskID, input.Name, input.Site, input.Region, input.Endpoint, input.Build, input.Capacity).Scan(&out.NodeID)
			if errors.Is(err, pgx.ErrNoRows) {
				return errTaskConflict
			}
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO task_targets(task_id,object_key,state,result_code)
				VALUES($1,$2,'waiting','relay_pending_enrollment')`, out.TaskID, out.NodeID); err != nil {
				return err
			}
		} else {
			var registered bool
			err := tx.QueryRow(ctx, `SELECT task_id,registered_at IS NOT NULL FROM relay_nodes
				WHERE id=$1 AND organization_id=$2 FOR UPDATE`, node, who.Organization).Scan(&out.TaskID, &registered)
			if errors.Is(err, pgx.ErrNoRows) {
				return errTaskMissing
			}
			if err != nil {
				return err
			}
			if registered {
				return errTaskConflict
			}
		}
		if err := tx.QueryRow(ctx, `INSERT INTO relay_enrollments(node_id,credential_digest,expires_at)
			VALUES($1,$2,clock_timestamp()+interval '15 minutes')
			ON CONFLICT (node_id) DO UPDATE SET credential_digest=EXCLUDED.credential_digest,
			expires_at=EXCLUDED.expires_at,challenge=NULL,challenge_key=NULL,challenge_expires_at=NULL
			RETURNING expires_at`, out.NodeID, digest(out.Credential)).Scan(&out.Expires); err != nil {
			return err
		}
		_, err = addAuditEvent(ctx, tx, auditEvent{OrganizationID: who.Organization, ActorID: who.ID,
			TaskID: out.TaskID, ResourceID: out.NodeID, Action: "relay.enrollment_issued", Result: "accepted"})
		return err
	})
	if err != nil {
		failure(w, "issue relay enrollment", err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, out)
}

type relayEnrollmentState struct {
	relayprotocol.Configuration
	organization, task string
	challenge          string
	expiryMS           int64
	key                []byte
	current            bool
}

func lockRelayEnrollment(ctx context.Context, tx pgx.Tx, id string, credential []byte) (relayEnrollmentState, error) {
	var state relayEnrollmentState
	// Credential rotation uses the same node-first ordering as enrollment.
	err := tx.QueryRow(ctx, `SELECT id,organization_id,task_id,site,region,endpoint,build,capacity,config_version
		FROM relay_nodes WHERE id=$1 AND registered_at IS NULL FOR UPDATE`, id).Scan(
		&state.NodeID, &state.organization, &state.task, &state.Site, &state.Region, &state.Endpoint, &state.Build,
		&state.Capacity, &state.Version)
	if errors.Is(err, pgx.ErrNoRows) {
		return state, errManagedIdentity
	}
	if err != nil {
		return state, err
	}
	err = tx.QueryRow(ctx, `SELECT COALESCE(challenge,''),COALESCE(floor(extract(epoch FROM challenge_expires_at)*1000)::bigint,0),
		challenge_key,COALESCE(challenge_expires_at>clock_timestamp(),false)
		FROM relay_enrollments WHERE node_id=$1 AND credential_digest=$2 AND consumed_at IS NULL
		AND expires_at>clock_timestamp() FOR UPDATE`, id, credential).Scan(&state.challenge, &state.expiryMS, &state.key, &state.current)
	if errors.Is(err, pgx.ErrNoRows) {
		err = errManagedIdentity
	}
	return state, err
}

func (s relayEnrollmentState) proof() relayprotocol.Enrollment {
	return relayprotocol.Enrollment{Configuration: s.Configuration, Challenge: s.challenge, ExpiresMS: s.expiryMS,
		PublicKey: base64.RawURLEncoding.EncodeToString(s.key)}
}

func (a *API) relayEnroll(w http.ResponseWriter, r *http.Request) {
	credential := managedBearer(r, relayEnrollmentPrefix)
	if credential == nil {
		failure(w, "relay enrollment", errManagedIdentity)
		return
	}
	var in struct {
		NodeID    string `json:"node_id"`
		PublicKey string `json:"public_key"`
		Signature string `json:"signature,omitempty"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	key := relayprotocol.PublicKey(in.PublicKey)
	complete := r.URL.Path == "/v1/relays/enrollment/complete"
	if !userIDPattern.MatchString(in.NodeID) || key == nil || complete != (in.Signature != "") {
		failure(w, "relay enrollment", errManagedIdentity)
		return
	}
	ctx := r.Context()
	var out relayEnrollmentState
	err := pgx.BeginFunc(ctx, a.store.DB, func(tx pgx.Tx) error {
		state, err := lockRelayEnrollment(ctx, tx, in.NodeID, credential)
		if err != nil {
			return err
		}
		out = state
		if state.key != nil && !bytes.Equal(key, state.key) {
			return errManagedIdentity
		}
		if !complete {
			if state.current {
				return nil
			}
			out.challenge, err = token()
			if err != nil {
				return err
			}
			out.key = key
			return tx.QueryRow(ctx, `UPDATE relay_enrollments SET challenge=$2,challenge_key=$3,
				challenge_expires_at=LEAST(expires_at,clock_timestamp()+interval '2 minutes')
				WHERE node_id=$1 RETURNING floor(extract(epoch FROM challenge_expires_at)*1000)::bigint`,
				state.NodeID, out.challenge, key).Scan(&out.expiryMS)
		}
		if !state.current || !relayprotocol.Verify(state.key, state.proof().Frame(), in.Signature) {
			return errManagedIdentity
		}
		tag, err := tx.Exec(ctx, `UPDATE relay_enrollments SET consumed_at=clock_timestamp()
			WHERE node_id=$1 AND expires_at>clock_timestamp() AND challenge_expires_at>clock_timestamp()`, state.NodeID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return errManagedIdentity
		}
		if _, err := tx.Exec(ctx, `UPDATE relay_nodes SET identity_key=$2,registered_at=clock_timestamp()
			WHERE id=$1`, state.NodeID, key); err != nil {
			var constraint *pgconn.PgError
			if errors.As(err, &constraint) && constraint.ConstraintName == "relay_nodes_identity_key_key" {
				return errTaskConflict
			}
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE task_targets SET result_code='relay_pending_probe',version=version+1
			WHERE task_id=$1 AND object_key=$2`, state.task, state.NodeID); err != nil {
			return err
		}
		_, err = addAuditEvent(ctx, tx, auditEvent{OrganizationID: state.organization, TaskID: state.task, ResourceID: state.NodeID,
			Action: "relay.registered", Result: "accepted", Source: "relay_enrollment", SourceKey: state.NodeID})
		return err
	})
	if err != nil {
		failure(w, "relay enrollment", err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if complete {
		writeJSON(w, http.StatusOK, out.Configuration)
	} else {
		writeJSON(w, http.StatusOK, out.proof())
	}
}

func (a *API) relayHealth(w http.ResponseWriter, r *http.Request) {
	var in struct {
		relayprotocol.Health
		Signature string `json:"signature"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if !userIDPattern.MatchString(in.NodeID) || !relayBuild.MatchString(in.Build) ||
		in.Sequence < 1 || in.ConfigVersion < 1 || in.ActiveSessions < 0 || in.ActiveSessions > 100000 {
		failure(w, "relay health", errTaskInvalid)
		return
	}
	ctx := r.Context()
	err := pgx.BeginFunc(ctx, a.store.DB, func(tx pgx.Tx) error {
		var key []byte
		var version, previous int64
		var build string
		var capacity int
		var now time.Time
		err := tx.QueryRow(ctx, `SELECT identity_key,config_version,health_sequence,build,capacity,clock_timestamp()
			FROM relay_nodes WHERE id=$1 AND registered_at IS NOT NULL FOR UPDATE`, in.NodeID).
			Scan(&key, &version, &previous, &build, &capacity, &now)
		if errors.Is(err, pgx.ErrNoRows) {
			return errManagedIdentity
		}
		if err != nil {
			return err
		}
		if !relayprotocol.Verify(key, in.Health.Frame(), in.Signature) {
			return errManagedIdentity
		}
		window := relayTimestampWindow.Milliseconds()
		if in.ConfigVersion != version || in.Build != build || in.ActiveSessions > capacity ||
			in.Sequence <= previous || in.ObservedMS < now.UnixMilli()-window || in.ObservedMS > now.UnixMilli()+window {
			return errTaskConflict
		}
		_, err = tx.Exec(ctx, `UPDATE relay_nodes SET health_sequence=$2,health_at=$3,
			active_sessions=$4,protocol_ready=$5,health_config_version=config_version WHERE id=$1`,
			in.NodeID, in.Sequence, now, in.ActiveSessions, in.ProtocolReady)
		return err
	})
	if err != nil {
		failure(w, "relay health", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) relayConfiguration(w http.ResponseWriter, r *http.Request) {
	var in struct {
		relayprotocol.ConfigRequest
		Signature string `json:"signature"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if !userIDPattern.MatchString(in.NodeID) || !validToken(in.Nonce) {
		failure(w, "relay configuration", errManagedIdentity)
		return
	}
	var out relayprotocol.Configuration
	var key []byte
	var now time.Time
	err := a.store.DB.QueryRow(r.Context(), `SELECT id,site,region,endpoint,build,capacity,config_version,identity_key,clock_timestamp()
		FROM relay_nodes WHERE id=$1 AND registered_at IS NOT NULL`, in.NodeID).Scan(
		&out.NodeID, &out.Site, &out.Region, &out.Endpoint, &out.Build, &out.Capacity, &out.Version, &key, &now)
	if errors.Is(err, pgx.ErrNoRows) {
		err = errManagedIdentity
	}
	if err != nil {
		failure(w, "relay configuration", err)
		return
	}
	window := relayTimestampWindow.Milliseconds()
	if in.ObservedMS < now.UnixMilli()-window || in.ObservedMS > now.UnixMilli()+window ||
		!relayprotocol.Verify(key, in.ConfigRequest.Frame(), in.Signature) {
		failure(w, "relay configuration", errManagedIdentity)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

type relayNodeSummary struct {
	relayprotocol.Configuration
	Name           string     `json:"name"`
	TaskID         string     `json:"task_id"`
	State          string     `json:"state"`
	HealthAt       *time.Time `json:"health_at,omitempty"`
	ActiveSessions *int       `json:"active_sessions,omitempty"`
	Mode           string     `json:"mode"`
	Revision       int64      `json:"revision"`
	OperationTask  string     `json:"operation_task_id,omitempty"`
}

const relaySummaryFields = `id,endpoint,site,region,build,config_version,capacity,name,task_id,` +
	relayStateSQL + `,health_at,active_sessions,mode,revision,COALESCE(last_operation_task_id::text,'')`

func readRelaySummary(row pgx.Row) (relayNodeSummary, error) {
	var node relayNodeSummary
	err := row.Scan(&node.NodeID, &node.Endpoint, &node.Site, &node.Region, &node.Build, &node.Version, &node.Capacity,
		&node.Name, &node.TaskID, &node.State, &node.HealthAt, &node.ActiveSessions, &node.Mode, &node.Revision, &node.OperationTask)
	return node, err
}

func (a *API) listRelayNodes(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	nodes := make([]relayNodeSummary, 0)
	err := a.withActor(r, false, func(tx pgx.Tx, who actor) error {
		if !who.PlatformAdmin {
			return errForbidden
		}
		rows, err := tx.Query(ctx, `SELECT `+relaySummaryFields+`
			FROM relay_nodes WHERE organization_id=$1 ORDER BY name,id`, who.Organization)
		if err != nil {
			return err
		}
		nodes, err = pgx.AppendRows(nodes, rows, func(row pgx.CollectableRow) (relayNodeSummary, error) {
			return readRelaySummary(row)
		})
		return err
	})
	if err != nil {
		failure(w, "list relay nodes", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"nodes": nodes})
}
