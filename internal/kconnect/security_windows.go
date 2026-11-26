package kconnect

import "golang.org/x/sys/windows"

// Protected DACL allows only the current owner; files inherit that restriction.
func secureDirectory(path string) error {
	sd, err := windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;OW)")
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, dacl, nil)
}
