package bottle

import "io"

// CatalogInspection is the verified static result of inspecting a bottle for
// catalog ingestion. RuntimeDependencies are canonicalized, sorted, and
// transient: they are returned to the ingestion caller but are never serialized
// with the verification evidence. The embedded Result retains the exact Formula
// source as the existing transient materializer input.
type CatalogInspection struct {
	*Result
	RuntimeDependencies []ReceiptDependency `json:"-"`
}

// InspectForCatalog verifies a bottle using the same compressed-byte,
// hostile-archive, Formula, receipt-identity, and inventory checks as Verify,
// while discovering an embedded receipt's runtime_dependencies instead of
// comparing them with a predeclared list. Normal Homebrew bottles may omit the
// pre-install receipt; in that case the static archive and Formula checks still
// complete and RuntimeDependencies remains empty.
//
// Callers must leave Expectation.Dependencies empty. The discovered list is an
// ingestion result that must be bound into authenticated catalog metadata;
// normal Verify and VerifyNode calls continue to require an exact predeclared
// dependency match.
func InspectForCatalog(r io.Reader, expected Expectation, opts Options) (*CatalogInspection, error) {
	if len(expected.Dependencies) != 0 {
		return nil, verificationError(CodeInvalidExpectation, "", "catalog inspection discovers receipt dependencies; expected dependency list must be empty")
	}
	opts.Policy.RequirePreInstallReceipt = false
	var dependencies []ReceiptDependency
	result, err := verifyWithReceiptValidator(r, expected, opts, func(data []byte, normalized Expectation) (ReceiptEvidence, error) {
		evidence, discovered, err := discoverReceiptDependencies(data, normalized)
		if err != nil {
			return ReceiptEvidence{}, err
		}
		dependencies = discovered
		return evidence, nil
	})
	if err != nil {
		return nil, err
	}
	return &CatalogInspection{Result: result, RuntimeDependencies: dependencies}, nil
}
