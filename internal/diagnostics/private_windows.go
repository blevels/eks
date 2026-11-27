package diagnostics

import (
	"errors"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

func openPrivate(path string) (*os.File, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	sd, err := windows.SecurityDescriptorFromString("O:" + user.User.Sid.String() + "D:P(A;;FA;;;OW)")
	if err != nil {
		return nil, err
	}
	sa := &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	h, err := windows.CreateFile(p, windows.GENERIC_WRITE|windows.READ_CONTROL|windows.WRITE_DAC,
		0, sa, windows.OPEN_ALWAYS, windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(h), path)
	security, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	owner, _, err := security.Owner()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !owner.Equals(user.User.Sid) {
		_ = f.Close()
		return nil, errors.New("unsafe_log_owner")
	}
	var info windows.ByHandleFileInformation
	if err = windows.GetFileInformationByHandle(h, &info); err != nil || info.FileAttributes&(windows.FILE_ATTRIBUTE_REPARSE_POINT|windows.FILE_ATTRIBUTE_DIRECTORY) != 0 || info.NumberOfLinks != 1 {
		_ = f.Close()
		return nil, errors.New("unsafe_log_destination")
	}
	dacl, _, err := sd.DACL()
	if err == nil {
		err = windows.SetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
	}
	if err == nil {
		_, err = f.Seek(0, 2)
	}
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// openPrivate uses share mode zero, retaining an exclusive sidecar handle.
func lockPrivate(path string) (*os.File, error) { return openPrivate(path) }
