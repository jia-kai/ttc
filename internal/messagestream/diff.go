package messagestream

// Match identifies one unchanged occurrence in the old and current sequences.
// Matches are strictly increasing on both axes, including repeated hashes.
type Match struct{ Old, Current int }

// DiffHashes finds a longest common subsequence of ordered item hashes. Equal
// prefixes/suffixes are trimmed first. Myers search is bounded to 2 million
// work steps and 1 million saved frontier cells; false means callers must show
// an explicit full-baseline reset, not treat partial matches as a valid diff.
func DiffHashes(old, current []string) ([]Match, bool) {
	const workLimit, memoryLimit = 2000000, 1000000
	prefix := 0
	for prefix < len(old) && prefix < len(current) && old[prefix] == current[prefix] {
		prefix++
	}
	endOld, endCurrent := len(old), len(current)
	for endOld > prefix && endCurrent > prefix && old[endOld-1] == current[endCurrent-1] {
		endOld--
		endCurrent--
	}
	a, b := old[prefix:endOld], current[prefix:endCurrent]
	matches := make([]Match, 0, min(len(old), len(current)))
	for i := 0; i < prefix; i++ {
		matches = append(matches, Match{i, i})
	}
	if len(a) > 0 && len(b) > 0 {
		// Each frontier stores only its active diagonals, bounding trace memory by
		// edit distance squared rather than allocating input-length rows.
		trace := [][]int{}
		work, cells := 0, 0
		found := false
		for d := 0; d <= len(a)+len(b); d++ {
			if cells+d+1 > memoryLimit {
				return nil, false
			}
			cells += d + 1
			row := make([]int, d+1)
			for j, k := 0, -d; k <= d; j, k = j+1, k+2 {
				work++
				if work > workLimit {
					return nil, false
				}
				x := 0
				if d > 0 {
					prev := trace[d-1]
					if k == -d || k != d && prev[j-1] < prev[j] {
						x = prev[j]
					} else {
						x = prev[j-1] + 1
					}
				}
				y := x - k
				for x < len(a) && y < len(b) && a[x] == b[y] {
					x++
					y++
					work++
					if work > workLimit {
						return nil, false
					}
				}
				row[j] = x
				if x == len(a) && y == len(b) {
					trace = append(trace, row)
					x, y = len(a), len(b)
					reverse := []Match{}
					for depth := d; depth > 0; depth-- {
						k := x - y
						j := (k + depth) / 2
						prev := trace[depth-1]
						prevK := k - 1
						if k == -depth || k != depth && prev[j-1] < prev[j] {
							prevK = k + 1
						}
						px := prev[(prevK+depth-1)/2]
						py := px - prevK
						for x > px && y > py {
							x--
							y--
							reverse = append(reverse, Match{prefix + x, prefix + y})
						}
						x, y = px, py
					}
					for x > 0 && y > 0 {
						x--
						y--
						reverse = append(reverse, Match{prefix + x, prefix + y})
					}
					for i := len(reverse) - 1; i >= 0; i-- {
						matches = append(matches, reverse[i])
					}
					found = true
					break
				}
			}
			if found {
				break
			}
			trace = append(trace, row)
		}
	}
	for i, j := endOld, endCurrent; i < len(old); i, j = i+1, j+1 {
		matches = append(matches, Match{i, j})
	}
	return matches, true
}
