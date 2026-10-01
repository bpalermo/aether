package cache

// Every snapshot build in this package's tests is a STRICT version-memo audit
// (issue #1105): each resource is re-hashed, memo hits included, and a memo
// hit whose bytes changed -- a builder mutated a published proto in place --
// panics the test that did it. The memo's soundness rests on that rule, so
// the whole suite, which drives every builder, is its audit. Production audits
// once a minute and counts instead (versionmemo.go).
func init() {
	versionMemoAuditEvery = 0
	versionMemoStrict = true
}
