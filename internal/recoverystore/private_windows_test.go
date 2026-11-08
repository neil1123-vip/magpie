package recoverystore

import (
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestRecoveryDirectoryUsesCurrentUserProtectedACL(t *testing.T) {
	s := testStore(t)
	sd, err := windows.GetNamedSecurityInfo(s.Dir, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	control, _, err := sd.Control()
	if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
		t.Fatalf("recovery directory inherits a public ACL: %v, %v", control, err)
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	sddl := sd.String()
	if strings.Count(sddl, "(A;") != 1 || !strings.Contains(sddl, user.User.Sid.String()) {
		t.Fatalf("directory is not limited to current user: %s", sddl)
	}
}
