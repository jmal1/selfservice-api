package vcenter

import (
	"errors"
	"testing"
)

func TestIsVMMoref(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"vm-8942", true},
		{"vm-1", true},
		{"vm-", false},
		{"vm-abc", false},
		{"vm-12a", false},
		{"student-windows-11", false},
		{"", false},
		{"VM-8942", false},
		{"vm-8942 ", false},
		{"host-12", false},
	}

	for _, c := range cases {
		if got := isVMMoref(c.in); got != c.want {
			t.Errorf("isVMMoref(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestDestroyVMAlreadyGoneErrorsAreRecognized(t *testing.T) {
	for _, message := range []string{
		"the object has already been deleted or has not been completely created",
		"ServerFaultCode: ManagedObjectNotFound: the object could not be found",
	} {
		err := errors.New(message)
		if !isAlreadyDeletedErr(err) && !isResourceNotFoundErr(err) {
			t.Fatalf("already-gone VM error was not recognized: %q", message)
		}
	}
}

func TestDuplicateCloneNameErrorsAreRecognized(t *testing.T) {
	for _, message := range []string{
		"ServerFaultCode: DuplicateName",
		"duplicate name in target folder",
		"the object already exists",
	} {
		if !isDuplicateNameErr(errors.New(message)) {
			t.Fatalf("duplicate clone name error was not recognized: %q", message)
		}
	}
}
