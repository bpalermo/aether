// Package must provides panic-on-error helpers for programming errors
// that should never occur at runtime, such as invalid flag registrations
// or failed type assertions on known types.
package must

// NoError panics if err is not nil. Use this only for programming errors
// that indicate a bug rather than a runtime failure.
func NoError(err error) {
	if err != nil {
		panic(err)
	}
}
