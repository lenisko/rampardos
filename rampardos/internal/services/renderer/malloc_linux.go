package renderer

/*
#include <malloc.h>
*/
import "C"

// releaseNativeMemory hands free memory in every glibc malloc arena back
// to the OS. A retired worker's threads leave their freed allocations in
// the arenas they used, and glibc returns little of it by itself, so RSS
// stepped up with every grow/shrink cycle. Trimming once per retirement
// keeps that flat without capping the arena count, which costs render
// latency: with all native threads sharing two arenas, renders contend
// on malloc even when only one is running.
func releaseNativeMemory() {
	C.malloc_trim(0)
}
