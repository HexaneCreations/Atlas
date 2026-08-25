package main

import "testing"

func TestValidateOperationAcceptsContainerLogs(t *testing.T) {
	if err := validateOperation("container_logs"); err != nil {
		t.Errorf("validateOperation(container_logs) = %v, want nil", err)
	}
}

func TestValidateOperationRejectsEmpty(t *testing.T) {
	if err := validateOperation(""); err == nil {
		t.Error("validateOperation(\"\") succeeded, want an error")
	}
}

func TestValidateOperationRejectsUnknown(t *testing.T) {
	if err := validateOperation("delete_everything"); err == nil {
		t.Error("validateOperation(delete_everything) succeeded, want an error")
	}
}
