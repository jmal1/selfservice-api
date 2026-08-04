package handlers

import (
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/jmal1/selfservice-api/internal/models"
)

func TestValidateTestingRunAccess(t *testing.T) {
	ownerID := uuid.New()
	otherID := uuid.New()

	tests := []struct {
		name       string
		podOwnerID uuid.UUID
		userID     uuid.UUID
		role       string
		want       int
	}{
		{
			name:       "student own allowed",
			podOwnerID: ownerID,
			userID:     ownerID,
			role:       models.RoleStudent,
			want:       0,
		},
		{
			name:       "student other denied",
			podOwnerID: ownerID,
			userID:     otherID,
			role:       models.RoleStudent,
			want:       http.StatusForbidden,
		},
		{
			name:       "instructor other allowed",
			podOwnerID: ownerID,
			userID:     otherID,
			role:       models.RoleInstructor,
			want:       0,
		},
		{
			name:       "admin other allowed",
			podOwnerID: ownerID,
			userID:     otherID,
			role:       models.RoleAdmin,
			want:       0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := validateTestingRunAccess(tc.podOwnerID, tc.userID, tc.role); got != tc.want {
				t.Fatalf("validateTestingRunAccess() = %d, want %d", got, tc.want)
			}
		})
	}
}
