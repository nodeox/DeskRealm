package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"unsafe"

	"deskrealm/internal/windowssetup"

	"golang.org/x/sys/windows"
)

func install() error {
	arch := "x64"
	switch runtime.GOARCH {
	case "arm64":
		arch = "arm64"
	case "amd64":
	default:
		return errors.New("installation requires Windows x64 or ARM64")
	}
	name, err := os.Executable()
	if err != nil {
		return err
	}
	image, err := os.Open(name)
	if err != nil {
		return err
	}
	defer image.Close()
	if err := verifySignature(image); err != nil {
		return fmt.Errorf("installer Authenticode signature was rejected: %w", err)
	}
	base, err := windows.KnownFolderPath(windows.FOLDERID_LocalAppData, 0)
	if err != nil {
		return err
	}
	directory, err := windowssetup.Install(image, arch, filepath.Join(base, "Programs", "DeskRealm", "releases"))
	if err != nil {
		return err
	}
	if err := installShortcut(directory); err != nil {
		return err
	}
	fmt.Println("DeskRealm installed at", directory)
	return nil
}

func verifySignature(file *os.File) error {
	name, err := windows.UTF16PtrFromString(file.Name())
	if err != nil {
		return err
	}
	info := windows.WinTrustFileInfo{Size: uint32(unsafe.Sizeof(windows.WinTrustFileInfo{})),
		FilePath: name, File: windows.Handle(file.Fd())}
	data := windows.WinTrustData{
		Size: uint32(unsafe.Sizeof(windows.WinTrustData{})), UIChoice: windows.WTD_UI_NONE,
		RevocationChecks: windows.WTD_REVOKE_NONE, UnionChoice: windows.WTD_CHOICE_FILE,
		StateAction: windows.WTD_STATEACTION_VERIFY, FileOrCatalogOrBlobOrSgnrOrCert: unsafe.Pointer(&info),
		ProvFlags: windows.WTD_DISABLE_MD2_MD4, UIContext: windows.WTD_UICONTEXT_INSTALL,
	}
	verifyErr := windows.WinVerifyTrustEx(windows.InvalidHWND, &windows.WINTRUST_ACTION_GENERIC_VERIFY_V2, &data)
	data.StateAction = windows.WTD_STATEACTION_CLOSE
	closeErr := windows.WinVerifyTrustEx(windows.InvalidHWND, &windows.WINTRUST_ACTION_GENERIC_VERIFY_V2, &data)
	runtime.KeepAlive(file)
	return errors.Join(verifyErr, closeErr)
}

func installShortcut(directory string) error {
	programs, err := windows.KnownFolderPath(windows.FOLDERID_Programs, 0)
	if err != nil {
		return err
	}
	system, err := windows.GetSystemDirectory()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(programs, 0700); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(programs, ".deskrealm-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	shortcut := filepath.Join(stage, "DeskRealm.lnk")
	program := filepath.Join(system, "WindowsPowerShell", "v1.0", "powershell.exe")
	const script = `
$ErrorActionPreference = 'Stop'
$shell = New-Object -ComObject WScript.Shell
$link = $shell.CreateShortcut($env:DESKREALM_SETUP_SHORTCUT)
$link.TargetPath = Join-Path $env:DESKREALM_SETUP_DIR 'DeskRealm.exe'
$link.WorkingDirectory = $env:DESKREALM_SETUP_DIR
$link.IconLocation = Join-Path $env:DESKREALM_SETUP_DIR 'deskrealm-brand.ico'
$link.Save()
`
	cmd := exec.Command(program, "-NoProfile", "-NonInteractive", "-Command", script)
	cmd.Dir = system
	cmd.Env = append(cmd.Environ(), "DESKREALM_SETUP_SHORTCUT="+shortcut, "DESKREALM_SETUP_DIR="+directory)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("unable to create the DeskRealm start-menu shortcut: %w", err)
	}
	return os.Rename(shortcut, filepath.Join(programs, "DeskRealm.lnk"))
}
