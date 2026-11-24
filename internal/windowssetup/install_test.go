package windowssetup

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"deskrealm/internal/windowsdelivery"
)

func archiveBytes(t *testing.T, names []string, contents [][]byte) []byte {
	t.Helper()
	var out bytes.Buffer
	w := zip.NewWriter(&out)
	for i, name := range names {
		file, err := w.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Store})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write(contents[i]); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func testBundle(t *testing.T, name string) []byte {
	t.Helper()
	names := []string{"DeskRealm.exe", "rustdesk.exe", "librustdesk.dll", "data/app.so"}
	files := [][]byte{[]byte("launcher"), []byte("desktop"), []byte("library"), []byte("flutter")}
	if name != "" {
		names = append(names, name)
		files = append(files, []byte("unsafe"))
	}
	profile, err := json.Marshal(windowsdelivery.PublicProfile{Schema: 1, Version: "0.1.0-dev.1",
		Center: windowsdelivery.Center{
			Origin: "https://deskrealm.example.test", IDServer: "deskrealm.example.test:21116",
			RendezvousKey: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32)),
			SigningKey:    base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{2}, 32)), SigningKeyID: "center-1",
		}})
	if err != nil {
		t.Fatal(err)
	}
	return archiveBytes(t, []string{"client.zip", profileName, iconName}, [][]byte{archiveBytes(t, names, files), profile, []byte("icon")})
}

func TestInstallsExactTreeAndPreservesPriorVersion(t *testing.T) {
	root := t.TempDir()
	bundle := testBundle(t, "")
	sum := sha256.Sum256(bundle)
	first, err := installBundle(bytes.NewReader(bundle), int64(len(bundle)), sum, root)
	if err != nil {
		t.Fatal(err)
	}
	if next, err := installBundle(bytes.NewReader(bundle), int64(len(bundle)), sum, root); err != nil || next != first {
		t.Fatalf("retry changed installation: %s %v", next, err)
	}
	for _, name := range []string{"DeskRealm.exe", "rustdesk.exe", profileName, iconName, "data/app.so"} {
		if info, err := os.Stat(filepath.Join(first, name)); err != nil || !info.Mode().IsRegular() {
			t.Fatalf("installed file %s: %v", name, err)
		}
	}
	for _, name := range []string{"../escaped", "NUL.txt", "DeskRealm.exe ", "deskrealm.exe", "DATA/extra", "data", profileName} {
		unsafe := testBundle(t, name)
		if _, err := installBundle(bytes.NewReader(unsafe), int64(len(unsafe)), sha256.Sum256(unsafe), root); err == nil {
			t.Fatalf("accepted unsafe archive entry %q", name)
		}
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 || entries[0].Name() != filepath.Base(first) {
		t.Fatalf("unsafe inputs leaked partial installs: %v %v", entries, err)
	}
	if err := os.WriteFile(filepath.Join(first, "rustdesk.exe"), []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := installBundle(bytes.NewReader(bundle), int64(len(bundle)), sum, root); err == nil {
		t.Fatal("reused a modified installation")
	}
}

func TestChecksExactImageBeforeAuthenticodeVerification(t *testing.T) {
	dir := t.TempDir()
	stub, archive, unsigned, signed := filepath.Join(dir, "stub.exe"), filepath.Join(dir, "bundle.zip"),
		filepath.Join(dir, "unsigned.exe"), filepath.Join(dir, "signed.exe")
	bundle := testBundle(t, "")
	image := make([]byte, 1024)
	copy(image, "MZ")
	binary.LittleEndian.PutUint32(image[60:], 64)
	copy(image[64:], "PE\x00\x00")
	binary.LittleEndian.PutUint16(image[68:], machineFor("x64"))
	binary.LittleEndian.PutUint16(image[84:], 240)
	binary.LittleEndian.PutUint16(image[88:], 0x20b)
	binary.LittleEndian.PutUint32(image[88+108:], 16)
	if err := os.WriteFile(stub, image, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(archive, bundle, 0600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(bundle)
	if err := Assemble("arm64", stub, archive, hex.EncodeToString(sum[:]), unsigned); err == nil {
		t.Fatal("accepted a different stub architecture")
	}
	if err := Assemble("x64", stub, archive, hex.EncodeToString(sum[:]), unsigned); err != nil {
		t.Fatal(err)
	}
	prepared, err := os.ReadFile(unsigned)
	if err != nil {
		t.Fatal(err)
	}
	header, err := readPE(bytes.NewReader(prepared), int64(len(prepared)))
	if err != nil {
		t.Fatal(err)
	}
	withCertificate := append(prepared, make([]byte, 8)...)
	binary.LittleEndian.PutUint32(withCertificate[header.certField:], uint32(len(prepared)))
	binary.LittleEndian.PutUint32(withCertificate[header.certField+4:], 8)
	binary.LittleEndian.PutUint32(withCertificate[header.checksumField:], 1)
	write := func() {
		t.Helper()
		if err := os.WriteFile(signed, withCertificate, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write()
	if err := Check("x64", unsigned, signed); err != nil {
		t.Fatal(err)
	}
	withCertificate[1024+10] ^= 0xff
	write()
	if err := Check("x64", unsigned, signed); err == nil {
		t.Fatal("accepted a changed installer payload")
	}
}
