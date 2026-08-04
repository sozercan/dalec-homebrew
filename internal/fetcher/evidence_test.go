package fetcher

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDecodeAndVerifyEvidence(t *testing.T) {
	request := validRequest("payload")
	evidence := Evidence{SchemaVersion: EvidenceSchemaVersion, FetchPolicyVersion: FetchPolicyVersion, ArtifactID: request.ArtifactID, Filename: request.Filename, Size: request.ExpectedSize, SHA256: request.SHA256, RedactedHostSequence: []string{"origin.example.com"}}
	data, err := MarshalEvidence(evidence)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeEvidence(strings.NewReader(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyEvidence(decoded, request); err != nil {
		t.Fatal(err)
	}
	decoded.SHA256 = strings.Repeat("0", 64)
	if err := VerifyEvidence(decoded, request); err == nil {
		t.Fatal("tampered evidence accepted")
	}
}

func TestDecodeEvidenceRejectsDuplicateMembers(t *testing.T) {
	if _, err := DecodeEvidence(strings.NewReader(`{"schema_version":"a","schema_version":"b"}`)); err == nil {
		t.Fatal("duplicate evidence member accepted")
	}
}

func TestDecodeRejectsCaseFoldedMemberAliases(t *testing.T) {
	request := validRequest("payload")
	data, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	aliased := strings.Replace(string(data), `"url":`, `"URL":`, 1)
	if _, err := DecodeRequest(strings.NewReader(aliased)); err == nil {
		t.Fatal("case-folded request member accepted")
	}
}

func TestVerifyEvidenceUsesNormalizedOriginHost(t *testing.T) {
	request := validRequest("payload")
	request.URL = "https://Origin.Example.Com/bottles/widget.tar.gz"
	evidence := Evidence{SchemaVersion: EvidenceSchemaVersion, FetchPolicyVersion: FetchPolicyVersion, ArtifactID: request.ArtifactID, Filename: request.Filename, Size: request.ExpectedSize, SHA256: request.SHA256, RedactedHostSequence: []string{"origin.example.com"}}
	if err := VerifyEvidence(evidence, request); err != nil {
		t.Fatal(err)
	}
}
