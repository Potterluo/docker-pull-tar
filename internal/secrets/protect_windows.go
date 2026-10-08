//go:build windows

package secrets

import (
	"encoding/base64"
	"fmt"
	"runtime"
	"syscall"
	"unsafe"
)

// protect_windows.go seals secrets with the Windows Data Protection API
// (DPAPI): CryptProtectData / CryptUnprotectData from crypt32.dll.
//
// DPAPI derives its key from the CURRENT USER ACCOUNT and keeps it in the
// user's profile, so:
//
//   - there is no key file to create, back up, lose or leak — the data
//     directory holds only ciphertext;
//   - app.db is useless on another machine, and useless to another user on
//     this machine (unprotecting as a different user fails);
//   - nothing is stored in this process either, so a fresh New on the same
//     machine+account opens what an earlier process sealed.
//
// The DLLs are reached through syscall.NewLazyDLL on purpose: the alternative
// (golang.org/x/sys/windows) would be a new module dependency for two
// functions. Nothing is imported from x/sys here.
//
// This file is the ONLY place in the repo that calls CryptProtectData, and
// there is deliberately no fallback: if DPAPI is unavailable (a session with
// no user profile, e.g. some service accounts), New fails loudly instead of
// quietly writing credentials in a weaker form.

// dataBlob mirrors the Win32 DATA_BLOB: a byte count plus a pointer to the
// bytes. It must stay in this exact layout — the Win32 side reads it by
// offset.
type dataBlob struct {
	cbData uint32
	pbData *byte
}

var (
	crypt32  = syscall.NewLazyDLL("crypt32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")

	procCryptProtectData   = crypt32.NewProc("CryptProtectData")
	procCryptUnprotectData = crypt32.NewProc("CryptUnprotectData")
	// DPAPI hands the output buffer back through LocalAlloc; only LocalFree
	// releases it. Leaking it leaks the secret into the process heap.
	procLocalFree = kernel32.NewProc("LocalFree")
)

// cryptProtectUIForbidden (CRYPTPROTECT_UI_FORBIDDEN) forbids any DPAPI
// dialog. The server and the desktop shell call this from a non-interactive
// context (and a GUI binary can pop no useful prompt), so a prompt could only
// hang the process.
const cryptProtectUIForbidden = 0x1

// dpapiEntropy is a fixed application entropy blob, mixed into every blob we
// produce. It is what makes our ciphertext OUR ciphertext: another
// application on the same machine, running as the same user, cannot feed our
// stored blob to CryptUnprotectData without knowing this string, even though
// DPAPI's own key is shared. It is a constant namespace, not a secret — the
// security still comes from the user's DPAPI master key.
var dpapiEntropy = []byte("github.com/Potterluo/docker-pull-tar/registry-credential/v1")

// dpapiDescription is optional metadata tagged onto every blob, visible to
// DPAPI tooling. It carries no secret.
var dpapiDescription, dpapiDescriptionErr = syscall.UTF16PtrFromString("DockerPull registry credential")

// dpapiProtector is the Windows Protector. It holds no state at all: the
// "state" is the user's DPAPI master key, which lives in the profile.
type dpapiProtector struct{}

// New returns the DPAPI protector. dataDir is accepted for interface
// symmetry and is deliberately unused: DPAPI needs no file, and creating
// one would be a lie about where the protection comes from.
//
// It probes the API once (seal+open a throwaway value) so that a machine
// where DPAPI cannot work fails at boot, with this error, instead of at the
// first attempt to save a credential.
func New(dataDir string) (Protector, error) {
	_ = dataDir
	for _, proc := range []*syscall.LazyProc{procCryptProtectData, procCryptUnprotectData, procLocalFree} {
		if err := proc.Find(); err != nil {
			return nil, fmt.Errorf("secrets: 无法加载 Windows DPAPI（%s）: %w", proc.Name, err)
		}
	}
	if dpapiDescriptionErr != nil {
		return nil, fmt.Errorf("secrets: 无法准备 DPAPI 描述字符串: %w", dpapiDescriptionErr)
	}
	p := dpapiProtector{}
	probe, err := p.Seal([]byte("dockerpull-dpapi-probe"))
	if err != nil {
		return nil, fmt.Errorf("secrets: Windows DPAPI 不可用: %w", err)
	}
	if _, err := p.Open(probe); err != nil {
		return nil, fmt.Errorf("secrets: Windows DPAPI 自检失败: %w", err)
	}
	return p, nil
}

// Describe reports the mechanism honestly: the key belongs to the current
// user account, so another user (or another machine) cannot read the
// ciphertext, but a process already running as this user can.
func (dpapiProtector) Describe() string { return "Windows DPAPI（当前用户）" }

func (dpapiProtector) Seal(plaintext []byte) (string, error) {
	in := blobOf(plaintext)
	entropy := blobOf(dpapiEntropy)
	var out dataBlob
	ok, _, callErr := procCryptProtectData.Call(
		uintptr(unsafe.Pointer(&in)),
		uintptr(unsafe.Pointer(dpapiDescription)),
		uintptr(unsafe.Pointer(&entropy)),
		0, // pvReserved
		0, // pPromptStruct
		cryptProtectUIForbidden,
		uintptr(unsafe.Pointer(&out)),
	)
	runtime.KeepAlive(plaintext)
	runtime.KeepAlive(dpapiEntropy)
	if ok == 0 {
		return "", fmt.Errorf("secrets: DPAPI 加密失败: %w", callErr)
	}
	defer freeBlob(&out)
	return base64.StdEncoding.EncodeToString(unsafe.Slice(out.pbData, int(out.cbData))), nil
}

func (dpapiProtector) Open(sealed string) ([]byte, error) {
	if sealed == "" {
		return nil, ErrEmptySealed
	}
	raw, err := base64.StdEncoding.DecodeString(sealed)
	if err != nil {
		return nil, fmt.Errorf("%w: 不是合法的 base64", ErrMalformed)
	}
	if len(raw) == 0 {
		return nil, ErrEmptySealed
	}
	in := blobOf(raw)
	entropy := blobOf(dpapiEntropy)
	var out dataBlob
	ok, _, callErr := procCryptUnprotectData.Call(
		uintptr(unsafe.Pointer(&in)),
		0, // ppszDataDescr — the description is not needed back
		uintptr(unsafe.Pointer(&entropy)),
		0, // pvReserved
		0, // pPromptStruct
		cryptProtectUIForbidden,
		uintptr(unsafe.Pointer(&out)),
	)
	runtime.KeepAlive(raw)
	runtime.KeepAlive(dpapiEntropy)
	if ok == 0 {
		return nil, fmt.Errorf("%w: %v", ErrDecrypt, callErr)
	}
	defer freeBlob(&out)
	plain := make([]byte, int(out.cbData))
	copy(plain, unsafe.Slice(out.pbData, int(out.cbData)))
	return plain, nil
}

// emptyBlobByte is what an empty DATA_BLOB points at. DPAPI rejects a NULL
// buffer outright on some builds even with cbData 0, and a package-level byte
// is always non-nil and never collectable, so the pointer handed to Win32
// cannot go stale while the call is in flight.
var emptyBlobByte byte

// blobOf points a DATA_BLOB at b. The caller must keep b alive across the
// Win32 call (runtime.KeepAlive), since the pointer travels as a uintptr.
func blobOf(b []byte) dataBlob {
	if len(b) == 0 {
		return dataBlob{pbData: &emptyBlobByte}
	}
	return dataBlob{cbData: uint32(len(b)), pbData: &b[0]}
}

// freeBlob releases a DPAPI output buffer. LocalFree returns NULL on
// success; a failure means the secret stayed in the process heap, which is
// not something we can fix here, but it is also not worth failing the call
// over (the plaintext has already been recovered).
func freeBlob(b *dataBlob) {
	if b.pbData == nil {
		return
	}
	procLocalFree.Call(uintptr(unsafe.Pointer(b.pbData)))
	b.pbData = nil
	b.cbData = 0
}
