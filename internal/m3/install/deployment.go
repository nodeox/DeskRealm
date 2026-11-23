package install

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

const (
	manifestName = "deployment.json"
	composeName  = "compose.json"
)

type deployment struct {
	Version   int               `json:"version"`
	Config    config            `json:"config"`
	Center    publicDeployment  `json:"center"`
	RunAs     string            `json:"run_as"`
	Checksums map[string]string `json:"checksums"`
}

type publicDeployment struct {
	ID                  string `json:"deployment_id"`
	Project             string `json:"compose_project"`
	ComposeFile         string `json:"compose_file"`
	PublicOrigin        string `json:"public_origin"`
	IDServer            string `json:"id_server"`
	RendezvousKey       string `json:"rendezvous_public_key"`
	RendezvousKeySHA256 string `json:"rendezvous_public_key_sha256"`
	SigningKeyID        string `json:"signing_key_id"`
	SigningKey          string `json:"signing_public_key"`
	SigningKeySHA256    string `json:"signing_public_key_sha256"`
}

var requiredFiles = []string{
	composeName, "postgres-init.sql",
	"secrets/center.env", "secrets/migrate.env", "secrets/postgres.env", "secrets/rendezvous.env",
	"secrets/signing.seed", "rendezvous/id_ed25519", "rendezvous/id_ed25519.pub",
}

func prepareDeployment(c config) (publicDeployment, error) {
	info, err := os.Lstat(c.Directory)
	if err == nil {
		if !info.IsDir() {
			return publicDeployment{}, errors.New("deployment directory must be a real directory")
		}
		entries, err := os.ReadDir(c.Directory)
		if err != nil {
			return publicDeployment{}, err
		}
		if len(entries) > 0 {
			return readPrepared(c)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return publicDeployment{}, err
	}

	stage, err := os.MkdirTemp(filepath.Dir(c.Directory), ".deskrealm-prepare-")
	if err != nil {
		return publicDeployment{}, err
	}
	defer os.RemoveAll(stage)
	for _, name := range []string{"postgres", "rendezvous", "secrets"} {
		if err := os.Mkdir(filepath.Join(stage, name), 0700); err != nil {
			return publicDeployment{}, err
		}
	}
	d, files, err := newDeployment(c)
	if err != nil {
		return publicDeployment{}, err
	}
	for _, name := range requiredFiles {
		mode := os.FileMode(0600)
		if name == "postgres-init.sql" {
			mode = 0644
		}
		if err := writeSynced(filepath.Join(stage, name), files[name], mode); err != nil {
			return publicDeployment{}, fmt.Errorf("write deployment %s: %w", name, err)
		}
		d.Checksums[name] = sha256Hex(files[name])
	}
	manifest, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return publicDeployment{}, err
	}
	if err := writeSynced(filepath.Join(stage, manifestName), manifest, 0600); err != nil {
		return publicDeployment{}, err
	}
	for _, dir := range []string{"postgres", "rendezvous", "secrets", "."} {
		if err := syncDirectory(filepath.Join(stage, dir)); err != nil {
			return publicDeployment{}, err
		}
	}
	if err := os.Rename(stage, c.Directory); err != nil {
		// A concurrent prepare may already have published the entire deployment.
		if _, statErr := os.Lstat(filepath.Join(c.Directory, manifestName)); statErr == nil {
			return readPrepared(c)
		}
		return publicDeployment{}, fmt.Errorf("publish deployment: %w", err)
	}
	if err := syncDirectory(filepath.Dir(c.Directory)); err != nil {
		return publicDeployment{}, err
	}
	return d.Center, nil
}

func newDeployment(c config) (deployment, map[string][]byte, error) {
	d := deployment{
		Version: 1, Config: c, Checksums: make(map[string]string),
		RunAs: strconv.Itoa(os.Geteuid()) + ":" + strconv.Itoa(os.Getegid()),
	}
	var material [16 + 6*32]byte
	if _, err := rand.Read(material[:]); err != nil {
		return d, nil, err
	}
	identifier := hex.EncodeToString(material[:16])
	signing := ed25519.NewKeyFromSeed(material[16:48])
	signingPublic := signing.Public().(ed25519.PublicKey)
	routePublic, routePrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return d, nil, err
	}
	d.Center = describeDeployment(c, identifier, routePublic, signingPublic)
	totp := base64.StdEncoding.EncodeToString(material[48:80])
	rendezvousToken := base64.RawURLEncoding.EncodeToString(material[80:112])
	dbPasswords := map[string]string{
		"postgres":          hex.EncodeToString(material[176:208]),
		"deskrealm_migrate": hex.EncodeToString(material[112:144]),
		"deskrealm":         hex.EncodeToString(material[144:176]),
	}
	environment := func(user string) string {
		return "DESKREALM_DATABASE_URL=postgres://" + user + ":" + dbPasswords[user] + "@database:5432/deskrealm?sslmode=disable\n"
	}
	files := map[string][]byte{
		"postgres-init.sql": []byte(postgresInit),
		"secrets/postgres.env": []byte("POSTGRES_PASSWORD=" + dbPasswords["postgres"] +
			"\nDESKREALM_DB_MIGRATION_PASSWORD=" + dbPasswords["deskrealm_migrate"] +
			"\nDESKREALM_DB_PASSWORD=" + dbPasswords["deskrealm"] + "\n"),
		"secrets/center.env": []byte(environment("deskrealm") + "DESKREALM_TOTP_KEY=" + totp +
			"\nDESKREALM_MANAGED_RENDEZVOUS_TOKEN=" + rendezvousToken + "\n"),
		"secrets/migrate.env":       []byte(environment("deskrealm_migrate")),
		"secrets/rendezvous.env":    []byte("DESKREALM_MANAGED_RENDEZVOUS_TOKEN=" + rendezvousToken + "\n"),
		"secrets/signing.seed":      []byte(base64.RawURLEncoding.EncodeToString(signing.Seed()) + "\n"),
		"rendezvous/id_ed25519":     []byte(base64.StdEncoding.EncodeToString(routePrivate)),
		"rendezvous/id_ed25519.pub": []byte(d.Center.RendezvousKey),
	}
	files[composeName], err = renderCompose(d)
	return d, files, err
}

func describeDeployment(c config, id string, route, signing ed25519.PublicKey) publicDeployment {
	return publicDeployment{
		ID: id, Project: "deskrealm-center-" + id, ComposeFile: filepath.Join(c.Directory, composeName),
		PublicOrigin: c.PublicOrigin, IDServer: c.IDServer,
		RendezvousKey: base64.StdEncoding.EncodeToString(route), RendezvousKeySHA256: sha256Hex(route),
		SigningKeyID: "center-" + id[:16],
		SigningKey:   base64.RawURLEncoding.EncodeToString(signing), SigningKeySHA256: sha256Hex(signing),
	}
}

func readPrepared(c config) (publicDeployment, error) {
	var d deployment
	failed := func(name string) (publicDeployment, error) {
		return publicDeployment{}, fmt.Errorf("deployment is incomplete or altered at %s; restore the original files", name)
	}
	data, err := readSmallRegular(filepath.Join(c.Directory, manifestName), true)
	if err != nil || json.Unmarshal(data, &d) != nil || d.Version != 1 {
		return failed(manifestName)
	}
	if d.Config != c {
		return publicDeployment{}, errors.New("requested configuration differs from the persisted deployment")
	}
	for _, name := range []string{"postgres", "rendezvous", "secrets"} {
		info, err := os.Lstat(filepath.Join(c.Directory, name))
		if err != nil || !info.IsDir() {
			return failed(name)
		}
	}
	if info, _ := os.Lstat(filepath.Join(c.Directory, "secrets")); info == nil || info.Mode().Perm()&0077 != 0 {
		return failed("secrets permissions")
	}
	files := make(map[string][]byte, len(requiredFiles))
	for _, name := range requiredFiles {
		data, err := readSmallRegular(filepath.Join(c.Directory, name), name != "postgres-init.sql")
		if err != nil || sha256Hex(data) != d.Checksums[name] {
			return failed(name)
		}
		files[name] = data
	}
	identifier, err := hex.DecodeString(d.Center.ID)
	if err != nil || len(identifier) != 16 || hex.EncodeToString(identifier) != d.Center.ID {
		return failed(manifestName)
	}
	public, err := base64.StdEncoding.Strict().DecodeString(string(files["rendezvous/id_ed25519.pub"]))
	if err != nil || len(public) != ed25519.PublicKeySize {
		return failed("rendezvous/id_ed25519.pub")
	}
	seed, err := base64.RawURLEncoding.Strict().DecodeString(string(bytes.TrimSuffix(files["secrets/signing.seed"], []byte("\n"))))
	if err != nil || len(seed) != ed25519.SeedSize {
		return failed("secrets/signing.seed")
	}
	signing := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
	if d.Center != describeDeployment(c, d.Center.ID, public, signing) {
		return failed(manifestName)
	}
	rendered, err := renderCompose(d)
	if err != nil || !bytes.Equal(rendered, files[composeName]) {
		return failed(composeName)
	}
	return d.Center, nil
}

func readSmallRegular(path string, private bool) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > 1<<20 || (private && info.Mode().Perm()&0077 != 0) {
		return nil, errors.New("deployment metadata must be a regular file with the expected permissions")
	}
	return os.ReadFile(path)
}

func sha256Hex(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func writeSynced(path string, data []byte, mode os.FileMode) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if err := file.Chmod(mode); err != nil {
		return errors.Join(err, file.Close())
	}
	_, writeErr := file.Write(data)
	syncErr := file.Sync()
	return errors.Join(writeErr, syncErr, file.Close())
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}

const postgresInit = `\set ON_ERROR_STOP on
\getenv migrator_key DESKREALM_DB_MIGRATION_PASSWORD
\getenv runtime_key DESKREALM_DB_PASSWORD
BEGIN;
CREATE ROLE deskrealm_migrate LOGIN PASSWORD :'migrator_key';
CREATE ROLE deskrealm LOGIN PASSWORD :'runtime_key';
REVOKE CONNECT, TEMPORARY ON DATABASE deskrealm FROM PUBLIC;
GRANT CONNECT ON DATABASE deskrealm TO deskrealm_migrate, deskrealm;
GRANT USAGE, CREATE ON SCHEMA public TO deskrealm_migrate;
GRANT USAGE ON SCHEMA public TO deskrealm;
ALTER DEFAULT PRIVILEGES FOR ROLE deskrealm_migrate IN SCHEMA public
    GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO deskrealm;
ALTER DEFAULT PRIVILEGES FOR ROLE deskrealm_migrate IN SCHEMA public
    GRANT USAGE, SELECT ON SEQUENCES TO deskrealm;
COMMIT;
`
