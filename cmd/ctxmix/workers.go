package main

import "runtime"

// defaultWorkers caps the automatic choice: every worker needs about 100 MiB.
const defaultWorkers = 4

// workers returns the number of parallel blocks to use: the -j value, or the
// number of CPUs (at most defaultWorkers), but never more than the library allows.
func workers(j int) int {
	if j <= 0 {
		j = runtime.NumCPU()
		if j > defaultWorkers {
			j = defaultWorkers
		}
	}
	if j > 256 {
		j = 256
	}
	return j
}
