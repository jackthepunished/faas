package api

import (
	"encoding/json"
	"testing"
)

func TestExportAppEnvRequiresAcknowledgement(t *testing.T) {
	for _, body := range []string{`{}`, `{"acknowledge_sensitive_values":false}`} {
		var request ExportAppEnvRequest
		if err := json.Unmarshal([]byte(body), &request); err != nil {
			t.Fatal(err)
		}
		if request.Validate() == nil {
			t.Fatalf("accepted unacknowledged export %s", body)
		}
	}
	if problem := (ExportAppEnvRequest{AcknowledgeSensitiveValues: true}).Validate(); problem != nil {
		t.Fatal(problem)
	}
}

func TestAppEnvExportPreservesValues(t *testing.T) {
	original := AppEnvExportResponse{AppSlug: "app", Scope: "staging", Values: map[string]string{"EMPTY": "", "MULTILINE": "first\nsecond", "QUOTES": "\"quoted\"\\path"}}
	body, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	var decoded AppEnvExportResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	for key, value := range original.Values {
		if decoded.Values[key] != value {
			t.Fatalf("changed value for %s", key)
		}
	}
}
