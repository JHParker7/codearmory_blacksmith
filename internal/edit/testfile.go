package edit

import (
	"path"
	"strings"
)

// IsTestFile reports whether a path belongs to the specification author.
//
// IT USED TO BE THE _test.go SUFFIX ALONE, and the gap that leaves is not
// theoretical. Read off run 83: the developer needed helpers its tests could
// call, wrote them into server_test_helpers.go — which does not end in
// _test.go, so nothing stopped it — and imported testing and httptest. The
// harness then correctly refused the file as shipped code importing a testing
// package, and the developer spent 26 writes across three attempts trying to
// remove an import the file existed to use. Both directions fail: with the
// import it is production code importing testing, without it the helpers cannot
// take a *testing.T. The only move that works is a rename, which nothing
// suggested, and the attempt died on the refusal ceiling three times over.
//
// MATCHED BY SEGMENT, NOT BY SUBSTRING. "test" appears inside latest.go,
// contest.go, attestation.go and protest.go, all of them ordinary production
// names, and blocking those would trade one trap for another. Splitting on the
// separators a filename actually uses and asking whether any part IS test, or
// begins with it, keeps server_test_helpers.go and testdata.go while leaving
// latest.go alone.
//
// IT WIDENS BOTH WAYS, deliberately. The developer may no longer write these
// files, which is the point; the harness also attributes them to the author. Go
// still compiles anything not ending in _test.go into the package, so a helper
// under a name like this remains a real problem — it is just now a problem
// belonging to the stage that can rename it.
func IsTestFile(p string) bool {
	base := strings.ToLower(path.Base(path.Clean(p)))
	if !strings.HasSuffix(base, ".go") {
		return false
	}
	if strings.HasSuffix(base, "_test.go") {
		return true
	}
	for _, seg := range strings.FieldsFunc(strings.TrimSuffix(base, ".go"), isNameSeparator) {
		if seg == "test" || strings.HasPrefix(seg, "test") {
			return true
		}
	}
	return false
}

// isNameSeparator splits a file name into the parts a person reads it as.
func isNameSeparator(r rune) bool { return r == '_' || r == '-' || r == '.' }
