package install

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"deskrealm/internal/relayprotocol"
)

const relayUpgradeDir = "upgrades"
const relayUpgradeJournal = "runtime.json"

type relayRuntimeUpgrade struct {
	NodeID string `json:"node_id"`
	Active string `json:"active_sha256"`
	Task   string `json:"task_id,omitempty"`
	Target string `json:"target_sha256,omitempty"`
	State  string `json:"state"`
}

func (u relayRuntimeUpgrade) finished() bool {
	return u.State == "ready" || u.State == "succeeded" || u.State == "rolled_back" || u.State == "canceled"
}

func readRelayRuntime(dir, node string) (relayRuntimeUpgrade, error) {
	var u relayRuntimeUpgrade
	data, err := readSmallRegular(filepath.Join(dir, relayUpgradeJournal), true)
	if err != nil {
		return u, err
	}
	if decodeRelayResponse(data, &u) != nil || u.NodeID != node || !digestHex.MatchString(u.Active) {
		return u, errors.New("invalid relay upgrade journal")
	}
	if u.State == "ready" && u.Task == "" && u.Target == "" {
		return u, nil
	}
	if !relayNodeID.MatchString(u.Task) || !digestHex.MatchString(u.Target) {
		return u, errors.New("invalid relay upgrade task")
	}
	switch u.State {
	case "requested", "switching", "rolling_back", "succeeded", "rolled_back", "canceled":
		if (u.State == "succeeded") == (u.Active == u.Target) {
			return u, nil
		}
	}
	return u, errors.New("invalid relay upgrade phase")
}

func (u relayRuntimeUpgrade) save(dir string) error {
	data, err := json.MarshalIndent(u, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(dir, relayUpgradeJournal, data, 0600)
}

func relayBinarySHA(path string) (string, error) {
	in, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 {
		return "", errors.New("relay binary must be a regular nonempty file")
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, in); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// Install the verified bytes as an immutable, private filename. Keep the old
// executable available even if the original installation path is replaced.
func stageRelayBinary(dir, source, expected string) (string, error) {
	if !filepath.IsAbs(source) {
		return "", errors.New("relay binary path must be absolute")
	}
	in, err := os.Open(source)
	if err != nil {
		return "", err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 {
		return "", errors.New("relay binary must be a regular nonempty file")
	}
	staged, err := os.CreateTemp(dir, ".hbbr-")
	if err != nil {
		return "", err
	}
	defer os.Remove(staged.Name())
	hash := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(staged, hash), in)
	if err := errors.Join(copyErr, staged.Chmod(0500), staged.Sync(), staged.Close()); err != nil {
		return "", err
	}
	digest := hex.EncodeToString(hash.Sum(nil))
	if expected != "" && digest != expected {
		return "", errors.New("relay binary SHA-256 differs from the authorized digest")
	}
	if err := os.Link(staged.Name(), filepath.Join(dir, digest)); err != nil && !errors.Is(err, os.ErrExist) {
		return "", err
	}
	if _, err := checkedRelayBinary(dir, digest); err != nil {
		return "", err
	}
	return digest, syncDirectory(dir)
}

func checkedRelayBinary(dir, digest string) (string, error) {
	path := filepath.Join(dir, digest)
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0500 {
		return "", errors.New("verified relay executable is missing or has changed permissions")
	}
	sum, err := relayBinarySHA(path)
	if err != nil || sum != digest {
		return "", errors.New("verified relay executable has changed bytes")
	}
	return path, nil
}

func relayUpgradeCall(ctx context.Context, client *http.Client, state relayHost, action, task, digest string) (relayprotocol.Upgrade, error) {
	var out relayprotocol.Upgrade
	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return out, err
	}
	req := relayprotocol.UpgradeRequest{NodeID: state.NodeID, TaskID: task, ConfigVersion: state.Config.Version, Action: action,
		SHA256: digest, ObservedMS: time.Now().UnixMilli(), Nonce: base64.RawURLEncoding.EncodeToString(nonce[:])}
	key := ed25519.NewKeyFromSeed(state.Seed)
	err := relayPost(ctx, client, state.Center, "/v1/relays/upgrade", "", struct {
		relayprotocol.UpgradeRequest
		Signature string `json:"signature"`
	}{req, base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, req.Frame()))}, &out)
	if err == nil && (out.TaskID != task || !digestHex.MatchString(out.CandidateSHA)) {
		err = errors.New("center returned an unrelated relay upgrade")
	}
	return out, err
}

func requestRelayUpgrade(ctx context.Context, client *http.Client, dir string, node relayHost, task, binary, digest string) (relayRuntimeUpgrade, error) {
	var u relayRuntimeUpgrade
	if node.Config == nil || !relayNodeID.MatchString(task) || !digestHex.MatchString(digest) {
		return u, errors.New("upgrade requires an enrolled node, task ID and SHA-256")
	}
	lock, err := lockDeployment(dir)
	if err != nil {
		return u, errors.New("managed relay runtime is unavailable or busy")
	}
	defer lock.Close()
	u, err = readRelayRuntime(dir, node.NodeID)
	if err != nil {
		return u, err
	}
	if u.Task == task && u.Target == digest {
		return u, nil
	}
	if !u.finished() || digest == u.Active {
		return u, errors.New("relay already has the requested binary or an unfinished upgrade")
	}
	status, err := relayUpgradeCall(ctx, client, node, "status", task, digest)
	if err != nil {
		return u, err
	}
	if status.CandidateSHA != digest || status.State != "pending" {
		return u, errors.New("center has no pending authorization for the candidate digest")
	}
	if _, err := stageRelayBinary(dir, binary, digest); err != nil {
		return u, err
	}
	u.Task, u.Target, u.State = task, digest, "requested"
	return u, u.save(dir)
}

func prepareManagedRelayBinary(dir, source, node string) (relayRuntimeUpgrade, error) {
	var u relayRuntimeUpgrade
	if err := os.Mkdir(dir, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return u, err
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		return u, errors.New("relay upgrades require a private upgrades directory")
	}
	lock, err := lockDeployment(dir)
	if err != nil {
		return u, err
	}
	defer lock.Close()
	u, err = readRelayRuntime(dir, node)
	if !errors.Is(err, os.ErrNotExist) {
		return u, err
	}
	digest, err := stageRelayBinary(dir, source, "")
	if err != nil {
		return u, err
	}
	u = relayRuntimeUpgrade{NodeID: node, Active: digest, State: "ready"}
	return u, u.save(dir)
}
