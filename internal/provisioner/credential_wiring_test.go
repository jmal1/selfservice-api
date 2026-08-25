package provisioner

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jmal1/selfservice-api/internal/models"
)

type fakeCredentialAcceptanceStore struct {
	applied  bool
	err      error
	calls    int
	podVMID  uuid.UUID
	moref    string
	username string
	password string
}

func (f *fakeCredentialAcceptanceStore) MarkPodVMCredentialsVerified(
	_ context.Context,
	id uuid.UUID,
	moref, username, password string,
) (bool, error) {
	f.calls++
	f.podVMID = id
	f.moref = moref
	f.username = username
	f.password = password
	return f.applied, f.err
}

func TestCredentialAcceptance_RejectingGuestCompensatesBeforeMarker(t *testing.T) {
	podVMID := uuid.New()
	validator := &fakeGuestCredentialValidator{errs: []error{errors.New("guest rejected credential")}}
	store := &fakeCredentialAcceptanceStore{applied: true}
	state := models.VMStatusConfiguring
	compensations := 0

	err := enforcePodVMCredentialAcceptance(
		context.Background(),
		store,
		validator,
		podVMID,
		models.TemplateKindCloneWithCustomize,
		"linux",
		"vm-101",
		"student",
		"Generated1!",
		0,
		time.Millisecond,
		func(cause error) error {
			compensations++
			state = models.VMStatusError
			return cause
		},
	)

	if err == nil || !strings.Contains(err.Error(), "guest customization did not install") {
		t.Fatalf("rejecting guest error = %v", err)
	}
	if store.calls != 0 {
		t.Fatalf("acceptance marker written %d times after guest rejection", store.calls)
	}
	if compensations != 1 || state != models.VMStatusError {
		t.Fatalf("compensation calls/state = %d/%q, want 1/error", compensations, state)
	}
}

func TestCredentialAcceptance_MarkerSabotageCompensates(t *testing.T) {
	podVMID := uuid.New()
	validator := &fakeGuestCredentialValidator{}
	store := &fakeCredentialAcceptanceStore{applied: false}
	state := models.VMStatusConfiguring

	err := enforcePodVMCredentialAcceptance(
		context.Background(),
		store,
		validator,
		podVMID,
		models.TemplateKindCloneWithCustomize,
		"windows",
		"vm-202",
		"Student",
		"Generated2!",
		time.Second,
		time.Millisecond,
		func(cause error) error {
			state = models.VMStatusError
			return cause
		},
	)

	if err == nil || !strings.Contains(err.Error(), "no longer matches") {
		t.Fatalf("sabotaged marker error = %v", err)
	}
	if validator.calls != 1 || store.calls != 1 {
		t.Fatalf("validator/marker calls = %d/%d, want 1/1", validator.calls, store.calls)
	}
	if state != models.VMStatusError {
		t.Fatalf("state = %q, want error after marker sabotage", state)
	}
}

func TestCredentialAcceptance_SuccessPersistsExactPair(t *testing.T) {
	podVMID := uuid.New()
	validator := &fakeGuestCredentialValidator{}
	store := &fakeCredentialAcceptanceStore{applied: true}
	compensations := 0

	err := enforcePodVMCredentialAcceptance(
		context.Background(),
		store,
		validator,
		podVMID,
		models.TemplateKindCloneWithCustomize,
		"linux",
		"vm-303",
		"student",
		"Generated3!",
		time.Second,
		time.Millisecond,
		func(cause error) error {
			compensations++
			return cause
		},
	)

	if err != nil {
		t.Fatal(err)
	}
	if store.calls != 1 || store.podVMID != podVMID ||
		store.moref != "vm-303" ||
		store.username != "student" || store.password != "Generated3!" {
		t.Fatalf("acceptance marker did not bind the exact pair: %+v", store)
	}

	if compensations != 0 {
		t.Fatalf("successful acceptance compensated %d times", compensations)
	}
}

func TestCredentialAcceptance_ReplacementCloneCannotReuseOldMarker(t *testing.T) {
	oldMoref := "vm-old"
	newMoref := "vm-replacement"
	verifiedAt := time.Now()
	vm := &models.PodVM{
		VCenterVMID:                  &oldMoref,
		GuestCredentialsVerifiedAt:   &verifiedAt,
		GuestCredentialsVerifiedVMID: &oldMoref,
	}
	if !podVMCredentialAccepted(vm) {
		t.Fatal("old clone marker should initially match its exact VM identity")
	}

	vm.VCenterVMID = &newMoref
	if podVMCredentialAccepted(vm) {
		t.Fatal("replacement clone reused acceptance bound to the destroyed VM")
	}

	validator := &fakeGuestCredentialValidator{}
	store := &fakeCredentialAcceptanceStore{applied: true}
	if err := enforcePodVMCredentialAcceptance(
		context.Background(),
		store,
		validator,
		uuid.New(),
		models.TemplateKindCloneWithCustomize,
		"linux",
		newMoref,
		"student",
		"SamePersistedPassword1!",
		time.Second,
		time.Millisecond,
		nil,
	); err != nil {
		t.Fatal(err)
	}
	if validator.calls != 1 || store.moref != newMoref {
		t.Fatalf(
			"replacement validation calls/marker moref = %d/%q, want 1/%q",
			validator.calls,
			store.moref,
			newMoref,
		)
	}
}

func TestCredentialAcceptance_StaticKindBypassesGeneratedContract(t *testing.T) {
	validator := &fakeGuestCredentialValidator{errs: []error{errors.New("must not be called")}}
	store := &fakeCredentialAcceptanceStore{err: errors.New("must not be called")}
	compensations := 0

	err := enforcePodVMCredentialAcceptance(
		context.Background(),
		store,
		validator,
		uuid.New(),
		models.TemplateKindCloneNoCustomize,
		"linux",
		"vm-static",
		"admin",
		"Static1!",
		0,
		0,
		func(cause error) error {
			compensations++
			return cause
		},
	)

	if err != nil {
		t.Fatal(err)
	}
	if validator.calls != 0 || store.calls != 0 || compensations != 0 {
		t.Fatalf(
			"static contract invoked validator/marker/compensation = %d/%d/%d",
			validator.calls,
			store.calls,
			compensations,
		)
	}
}
