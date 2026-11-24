package m1

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
)

// RelayIngress accepts an overwritten single X-Forwarded-For value only from
// explicitly configured immediate ingress IPs.
type RelayIngress struct {
	proxies []netip.Addr
}

func ConfigureRelayIngress(value string) (RelayIngress, error) {
	var config RelayIngress
	if value == "" {
		return config, nil
	}
	for address := range strings.SplitSeq(value, ",") {
		ip, ok := relayIP(strings.TrimSpace(address))
		if !ok || slices.Contains(config.proxies, ip) {
			return RelayIngress{}, errors.New("relay trusted proxies must be distinct IP addresses")
		}
		config.proxies = append(config.proxies, ip)
	}
	return config, nil
}

func relayIP(value string) (netip.Addr, bool) {
	ip, err := netip.ParseAddr(value)
	if err != nil || ip.Zone() != "" {
		return netip.Addr{}, false
	}
	ip = ip.Unmap()
	return ip, !ip.IsUnspecified() && !ip.IsMulticast()
}

func (ingress RelayIngress) clientIP(r *http.Request) (netip.Addr, bool) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return netip.Addr{}, false
	}
	ip, ok := relayIP(host)
	if !ok || !slices.Contains(ingress.proxies, ip) {
		return ip, ok
	}
	forwarded := r.Header.Values("X-Forwarded-For")
	if len(forwarded) != 1 {
		return netip.Addr{}, false
	}
	return relayIP(strings.TrimSpace(forwarded[0]))
}

type relayNetworkPolicy struct {
	Revision int64               `json:"revision"`
	Mode     string              `json:"mode"`
	Networks []relaySiteNetwork  `json:"networks"`
	Pairs    []relaySitePair     `json:"site_pairs"`
	Relays   []relayReachability `json:"relay_networks"`
}

type relaySiteNetwork struct {
	Network netip.Prefix `json:"cidr"`
	Site    string       `json:"site"`
}

type relaySitePair struct {
	Controller string   `json:"controller_site"`
	Target     string   `json:"target_site"`
	Regions    []string `json:"regions"`
}

type relayReachability struct {
	Node     string         `json:"relay_id"`
	Networks []netip.Prefix `json:"cidrs"`
}

func validRelayNetworkPrefix(prefix netip.Prefix) bool {
	return prefix.IsValid() && !prefix.Addr().Is4In6() && prefix == prefix.Masked()
}

func (p relayNetworkPolicy) valid() bool {
	if p.Revision < 0 || len(p.Networks) > 64 || len(p.Pairs) > 32 || len(p.Relays) > 32 {
		return false
	}
	if p.Mode == "organization" {
		return len(p.Networks)+len(p.Pairs)+len(p.Relays) == 0
	}
	if p.Mode != "restricted" {
		return false
	}
	prefixes, sites := make(map[netip.Prefix]bool), make(map[string]bool)
	for _, network := range p.Networks {
		if !validRelayNetworkPrefix(network.Network) || !relayLabel.MatchString(network.Site) || prefixes[network.Network] {
			return false
		}
		prefixes[network.Network], sites[network.Site] = true, true
	}
	pairs := make(map[[2]string]bool)
	for _, pair := range p.Pairs {
		key := [2]string{pair.Controller, pair.Target}
		if !sites[pair.Controller] || !sites[pair.Target] || pairs[key] || len(pair.Regions) == 0 || len(pair.Regions) > 16 {
			return false
		}
		pairs[key] = true
		regions := make(map[string]bool)
		for _, region := range pair.Regions {
			if !relayLabel.MatchString(region) || regions[region] {
				return false
			}
			regions[region] = true
		}
	}
	nodes := make(map[string]bool)
	for _, relay := range p.Relays {
		if !userIDPattern.MatchString(relay.Node) || nodes[relay.Node] || len(relay.Networks) == 0 || len(relay.Networks) > 16 {
			return false
		}
		nodes[relay.Node] = true
		prefixes := make(map[netip.Prefix]bool)
		for _, prefix := range relay.Networks {
			if !validRelayNetworkPrefix(prefix) || prefixes[prefix] {
				return false
			}
			prefixes[prefix] = true
		}
	}
	return true
}

func (p relayNetworkPolicy) site(address netip.Addr) string {
	site, bits := "", -1
	for _, item := range p.Networks {
		if item.Network.Bits() > bits && item.Network.Contains(address) {
			site, bits = item.Site, item.Network.Bits()
		}
	}
	return site
}

type relayNetworkScope struct {
	regions []string
	nodes   []string
}

func (p relayNetworkPolicy) scope(controller, target netip.Addr) relayNetworkScope {
	scope := relayNetworkScope{regions: []string{}, nodes: []string{}}
	left, right := p.site(controller), p.site(target)
	for _, pair := range p.Pairs {
		if pair.Controller == left && pair.Target == right {
			scope.regions = pair.Regions
			break
		}
	}
	for _, relay := range p.Relays {
		contains := func(ip netip.Addr) bool {
			return slices.ContainsFunc(relay.Networks, func(prefix netip.Prefix) bool { return prefix.Contains(ip) })
		}
		if contains(controller) && contains(target) {
			scope.nodes = append(scope.nodes, relay.Node)
		}
	}
	return scope
}

func readRelayNetworkPolicy(ctx context.Context, tx pgx.Tx, org string) (relayNetworkPolicy, error) {
	var revision int64
	var policy relayNetworkPolicy
	err := tx.QueryRow(ctx, "SELECT revision,policy FROM relay_network_policies WHERE organization_id=$1", org).
		Scan(&revision, &policy)
	if errors.Is(err, pgx.ErrNoRows) {
		return relayNetworkPolicy{Mode: "organization"}, nil
	}
	policy.Revision = revision
	return policy, err
}

func (a *API) getRelayNetworkPolicy(w http.ResponseWriter, r *http.Request) {
	var out relayNetworkPolicy
	err := a.withActor(r, false, func(tx pgx.Tx, who actor) error {
		if !who.PlatformAdmin {
			return errForbidden
		}
		var err error
		out, err = readRelayNetworkPolicy(r.Context(), tx, who.Organization)
		return err
	})
	if err != nil {
		failure(w, "read relay network policy", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *API) saveRelayNetworkPolicy(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.sessionWrite(w, r); !ok {
		return
	}
	var policy relayNetworkPolicy
	if !readJSON(w, r, &policy) {
		return
	}
	if !policy.valid() {
		failure(w, "save relay network policy", errRelayInput)
		return
	}
	ctx := r.Context()
	err := a.withActor(r, true, func(tx pgx.Tx, who actor) error {
		if !who.PlatformAdmin {
			return errForbidden
		}
		// withActor(write=true) has already taken the organization lock.
		current, err := readRelayNetworkPolicy(ctx, tx, who.Organization)
		if err != nil {
			return err
		}
		if policy.Revision != current.Revision {
			return errRelayConflict
		}
		ids := make([]string, 0, len(policy.Relays))
		for _, node := range policy.Relays {
			ids = append(ids, node.Node)
		}
		var count int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM relay_nodes
			WHERE organization_id=$1 AND id=ANY($2::uuid[])`, who.Organization, ids).Scan(&count); err != nil {
			return err
		}
		if count != len(ids) {
			return errRelayInput
		}
		policy.Revision++
		_, err = tx.Exec(ctx, `INSERT INTO relay_network_policies(organization_id,revision,policy) VALUES($1,$2,$3)
			ON CONFLICT(organization_id) DO UPDATE SET revision=excluded.revision,policy=excluded.policy`,
			who.Organization, policy.Revision, policy)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE session_relay_rounds r SET superseded_at=statement_timestamp()
			FROM managed_sessions s WHERE s.id=r.session_id AND s.organization_id=$1 AND r.superseded_at IS NULL
				AND s.expires_at>statement_timestamp()
				AND NOT EXISTS(SELECT 1 FROM managed_session_redemptions d WHERE d.session_id=s.id)`, who.Organization); err != nil {
			return err
		}
		_, err = addAuditEvent(ctx, tx, auditEvent{OrganizationID: who.Organization, ActorID: who.ID,
			Action: "relay.network_policy_updated", Result: "success", Reason: policy.Mode})
		return err
	})
	if err != nil {
		failure(w, "save relay network policy", err)
		return
	}
	writeJSON(w, http.StatusOK, policy)
}
