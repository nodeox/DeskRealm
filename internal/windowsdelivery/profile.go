package windowsdelivery

import (
	"bytes"
	"encoding/base64"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

type PublicProfile struct {
	Schema  int    `json:"schema"`
	Version string `json:"version"`
	Brand   Brand  `json:"brand"`
	Center  Center `json:"center"`
}

func ParsePublicProfile(raw []byte) (PublicProfile, error) {
	var p PublicProfile
	if len(raw) > maxManifest || strictJSON(raw, &p) != nil || p.Schema != 1 ||
		len(p.Version) > 64 || !versionPattern.MatchString(p.Version) || !p.Center.validProfile() {
		return PublicProfile{}, ErrManifest
	}
	return p, nil
}

func (c Center) validProfile() bool {
	origin, err := url.Parse(c.Origin)
	if err != nil || origin.Scheme != "https" || origin.User != nil || origin.Host == "" ||
		c.Origin != "https://"+origin.Host || !profileOriginAuthority(origin) ||
		!profileAuthority(c.IDServer) || !idPattern.MatchString(c.SigningKeyID) {
		return false
	}
	rendezvous, err := base64.StdEncoding.Strict().DecodeString(c.RendezvousKey)
	signing, signErr := base64.RawURLEncoding.Strict().DecodeString(c.SigningKey)
	if err != nil || signErr != nil || len(rendezvous) != 32 || len(signing) != 32 ||
		base64.StdEncoding.EncodeToString(rendezvous) != c.RendezvousKey ||
		base64.RawURLEncoding.EncodeToString(signing) != c.SigningKey ||
		bytes.Equal(rendezvous, signing) || bytes.Equal(rendezvous, make([]byte, 32)) || bytes.Equal(signing, make([]byte, 32)) {
		return false
	}
	return c.DefaultRelay == nil || idPattern.MatchString(c.DefaultRelay.ID) && profileAuthority(c.DefaultRelay.Address)
}

func profileOriginAuthority(origin *url.URL) bool {
	if port := origin.Port(); port != "" {
		return port != "443" && profileAuthority(origin.Host)
	}
	host := origin.Hostname()
	if !profileHost(host) {
		return false
	}
	if strings.Contains(host, ":") {
		return origin.Host == "["+host+"]"
	}
	return origin.Host == host
}

func profileAuthority(authority string) bool {
	host, port, err := net.SplitHostPort(authority)
	number, numberErr := strconv.ParseUint(port, 10, 16)
	return err == nil && numberErr == nil && number > 0 && strconv.FormatUint(number, 10) == port &&
		profileHost(host) && net.JoinHostPort(host, port) == authority
}

func profileHost(host string) bool {
	if addr, err := netip.ParseAddr(host); err == nil {
		return !addr.IsUnspecified() && addr.Zone() == ""
	}
	if host == "" || len(host) > 253 {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' ||
			strings.IndexFunc(label, func(c rune) bool { return (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' }) >= 0 {
			return false
		}
	}
	return true
}
