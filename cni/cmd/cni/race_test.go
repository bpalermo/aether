//go:build race

package main

// raceInstrumented: the data binary is built with -race under --config=race,
// which roughly doubles its size; the bloat ceiling applies to plain builds only.
const raceInstrumented = true
