package database

import "testing"

func TestWorkflowEditRequiresReview(t *testing.T) {
	value := "changed"
	timeout := 60
	tests := []struct {
		name           string
		workflowName   *string
		description    *string
		category       *string
		script         *string
		setupScript    *string
		timeoutSeconds *int
		creationMode   *string
		want           bool
	}{
		{name: "empty patch", want: false},
		{name: "script", script: &value, want: true},
		{name: "metadata", description: &value, want: true},
		{name: "timeout", timeoutSeconds: &timeout, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := workflowEditRequiresReview(
				tt.workflowName,
				tt.description,
				tt.category,
				tt.script,
				tt.setupScript,
				tt.timeoutSeconds,
				tt.creationMode,
			)
			if got != tt.want {
				t.Fatalf("workflowEditRequiresReview() = %v, want %v", got, tt.want)
			}
		})
	}
}
