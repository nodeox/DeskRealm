// Package windowssetup assembles and installs DeskRealm's Windows executable installer.
package windowssetup

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"os"
)

const (
	marker       = "DeskRealmSetupV1"
	trailerSize  = 64
	maxPayload   = int64(1 << 30)
	maxImageSize = maxPayload + 64<<20
)

var errImage = errors.New("invalid DeskRealm installer image")

type peHeader struct {
	machine       uint16
	checksumField int64
	certField     int64
	certOffset    int64
	certSize      int64
}

func readPE(in io.ReaderAt, size int64) (peHeader, error) {
	var dos [64]byte
	if _, err := in.ReadAt(dos[:], 0); err != nil || string(dos[:2]) != "MZ" {
		return peHeader{}, errImage
	}
	offset := int64(binary.LittleEndian.Uint32(dos[60:]))
	const cert = 24 + 112 + 4*8
	var header [cert + 8]byte
	if offset < 64 || offset > 1<<20 || offset+int64(len(header)) > size {
		return peHeader{}, errImage
	}
	if _, err := in.ReadAt(header[:], offset); err != nil || string(header[:4]) != "PE\x00\x00" ||
		binary.LittleEndian.Uint16(header[20:]) < uint16(len(header)-24) ||
		binary.LittleEndian.Uint16(header[24:]) != 0x20b || binary.LittleEndian.Uint32(header[24+108:]) < 5 {
		return peHeader{}, errImage
	}
	p := peHeader{machine: binary.LittleEndian.Uint16(header[4:]), checksumField: offset + 24 + 64, certField: offset + cert,
		certOffset: int64(binary.LittleEndian.Uint32(header[cert:])), certSize: int64(binary.LittleEndian.Uint32(header[cert+4:]))}
	if p.certOffset != 0 || p.certSize != 0 {
		if p.certOffset < p.certField+8 || p.certOffset%8 != 0 || p.certSize < 8 || p.certOffset+p.certSize != size {
			return peHeader{}, errImage
		}
	}
	return p, nil
}

func machineFor(arch string) uint16 {
	switch arch {
	case "x64":
		return 0x8664
	case "arm64":
		return 0xaa64
	default:
		return 0
	}
}

type payload struct {
	offset, size int64
	digest       [sha256.Size]byte
}

func readPayload(in io.ReaderAt, end int64, p peHeader) (payload, error) {
	var trailer [trailerSize]byte
	if end > maxImageSize || end < p.certField+8+trailerSize {
		return payload{}, errImage
	}
	if _, err := in.ReadAt(trailer[:], end-trailerSize); err != nil || string(trailer[:16]) != marker {
		return payload{}, errImage
	}
	result := payload{offset: int64(binary.LittleEndian.Uint64(trailer[16:])), size: int64(binary.LittleEndian.Uint64(trailer[24:]))}
	copy(result.digest[:], trailer[32:])
	if result.offset < p.certField+8 || result.offset > end-trailerSize || result.size < 1 || result.size > maxPayload ||
		result.size > end-trailerSize-result.offset {
		return payload{}, errImage
	}
	padding := end - trailerSize - result.offset - result.size
	if padding > 7 {
		return payload{}, errImage
	}
	var pad [7]byte
	if _, err := in.ReadAt(pad[:padding], result.offset+result.size); err != nil || pad != [7]byte{} {
		return payload{}, errImage
	}
	hash := sha256.New()
	if n, err := io.Copy(hash, io.NewSectionReader(in, result.offset, result.size)); err != nil || n != result.size ||
		!bytes.Equal(hash.Sum(nil), result.digest[:]) {
		return payload{}, errImage
	}
	return result, nil
}

func regular(path string) (*os.File, int64, error) {
	in, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	info, err := in.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 {
		in.Close()
		return nil, 0, errImage
	}
	return in, info.Size(), nil
}

// Assemble appends a ZIP and a fixed trailer before the Authenticode certificate table.
func Assemble(arch, stub, archive, expected, output string) error {
	approved, err := hex.DecodeString(expected)
	if err != nil || len(approved) != sha256.Size {
		return errImage
	}
	exe, stubSize, err := regular(stub)
	if err != nil {
		return err
	}
	defer exe.Close()
	p, err := readPE(exe, stubSize)
	if err != nil || p.certSize != 0 || p.machine != machineFor(arch) || machineFor(arch) == 0 || stubSize > 64<<20 {
		return errImage
	}
	bundle, size, err := regular(archive)
	if err != nil {
		return err
	}
	defer bundle.Close()
	if size > maxPayload {
		return errImage
	}
	out, err := os.OpenFile(output, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	success := false
	defer func() {
		out.Close()
		if !success {
			_ = os.Remove(output)
		}
	}()
	if n, err := io.Copy(out, exe); err != nil || n != stubSize {
		return errImage
	}
	hash := sha256.New()
	if n, err := io.Copy(io.MultiWriter(out, hash), bundle); err != nil || n != size {
		return errImage
	}
	if !bytes.Equal(hash.Sum(nil), approved) {
		return errors.New("installer archive differs from the approved package")
	}
	if _, err := out.Write(make([]byte, (8-(stubSize+size)%8)%8)); err != nil {
		return err
	}
	var trailer [trailerSize]byte
	copy(trailer[:16], marker)
	binary.LittleEndian.PutUint64(trailer[16:], uint64(stubSize))
	binary.LittleEndian.PutUint64(trailer[24:], uint64(size))
	copy(trailer[32:], approved)
	if _, err := out.Write(trailer[:]); err != nil {
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	success = true
	return nil
}

// Check compares the signed installer to the prepared image, allowing changes
// only in the two PE fields that Authenticode necessarily updates.
func Check(arch, unsignedPath, signedPath string) error {
	unsigned, size, err := regular(unsignedPath)
	if err != nil {
		return err
	}
	defer unsigned.Close()
	signed, signedSize, err := regular(signedPath)
	if err != nil {
		return err
	}
	defer signed.Close()
	up, err := readPE(unsigned, size)
	if err != nil || up.certSize != 0 || up.machine != machineFor(arch) || machineFor(arch) == 0 {
		return errImage
	}
	sp, err := readPE(signed, signedSize)
	if err != nil || sp.machine != up.machine || sp.certOffset != size || sp.certSize == 0 ||
		sp.certField != up.certField || sp.checksumField != up.checksumField {
		return errImage
	}
	if _, err := readPayload(unsigned, size, up); err != nil {
		return err
	}
	left, right := sha256.New(), sha256.New()
	for _, region := range [][2]int64{{0, up.checksumField}, {up.checksumField + 4, up.certField}, {up.certField + 8, size}} {
		start, length := region[0], region[1]-region[0]
		for i, in := range []io.ReaderAt{unsigned, signed} {
			out := left
			if i == 1 {
				out = right
			}
			if n, err := io.Copy(out, io.NewSectionReader(in, start, length)); err != nil || n != length {
				return errImage
			}
		}
	}
	if !bytes.Equal(left.Sum(nil), right.Sum(nil)) {
		return errors.New("signed installer differs from the prepared image")
	}
	return nil
}
