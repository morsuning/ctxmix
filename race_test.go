//go:build race

package ctxmix

// raceEnabled slows the tests' timing assumptions down: the race detector
// makes the model roughly ten times slower.
const raceEnabled = true
