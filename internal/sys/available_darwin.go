package sys

// availableMB is the memory a new piece of work can have without macOS swapping, from vm_stat's
// page counts (in MB).
//
// "Inactive" is not free: on macOS it holds app memory as well as file cache, and app memory
// can only be reused by compressing it or writing it to swap. Counting all of it (as free +
// inactive does) overstated what was available by about 2x on a 16 GB Mac with swap 78% full.
// What can be had without swapping is:
//
//   - free pages ("Pages free" already leaves out speculative pages, which are file cache);
//   - file-backed pages: file cache, dropped at no cost (speculative pages are among them);
//   - purgeable pages: memory apps have marked as disposable;
//   - and what compressing idle app memory would free: the inactive pages that are not file
//     cache, times (1 - 1/ratio), at the compressor's current ratio.
//
// Active and inactive together are exactly the anonymous plus file-backed pages, so the file
// cache is never counted twice. Purgeable pages sit among the anonymous ones, and the inactive
// app memory is taken as inactive minus all file cache: a lower bound, so the compression credit
// is not overstated either.
func availableMB(mb func(string) float64) float64 {
	file := mb("File-backed pages")
	if file == 0 && mb("Anonymous pages") == 0 {
		// A vm_stat without the file-backed split (older macOS): the old estimate.
		return mb("Pages free") + mb("Pages inactive") + mb("Pages speculative")
	}
	ratio := 2.0
	if occ := mb("Pages occupied by compressor"); occ > 0 {
		ratio = min(max(mb("Pages stored in compressor")/occ, 1), 4)
	}
	idleApp := max(0, mb("Pages inactive")-file)
	return mb("Pages free") + file + mb("Pages purgeable") + idleApp*(1-1/ratio)
}
