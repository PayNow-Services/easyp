package gitmodules

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
)

type rememberedCandidate struct {
	Remote string `json:"remote"`
	Subdir string `json:"subdir"`
}

func rememberedCandidatePath(cacheRoot, source string) string {
	return filepath.Join(cacheRoot, "objects", "candidates", v1CacheSourceKey(source)+".json")
}

// preferRememberedCandidate moves the candidate that last served source to the
// front. A remembered value only reorders the computed candidates, so a stale
// or edited file never introduces a remote.
func preferRememberedCandidate(cacheRoot, source string, candidates []v1GitModuleCandidate) []v1GitModuleCandidate {
	raw, err := os.ReadFile(rememberedCandidatePath(cacheRoot, source))
	if err != nil {
		return candidates
	}
	var remembered rememberedCandidate
	err = json.Unmarshal(raw, &remembered)
	if err != nil {
		return candidates
	}
	index := slices.IndexFunc(candidates, func(candidate v1GitModuleCandidate) bool {
		return candidate.remote == remembered.Remote && candidate.subdir == remembered.Subdir
	})
	if index <= 0 {
		return candidates
	}
	ordered := []v1GitModuleCandidate{candidates[index]}
	ordered = append(ordered, candidates[:index]...)
	return append(ordered, candidates[index+1:]...)
}

// rememberCandidate records the candidate that served source. It is best
// effort: a failed write only costs the next cold install a wasted attempt.
func rememberCandidate(cacheRoot, source string, candidate v1GitModuleCandidate) {
	encoded, err := json.Marshal(rememberedCandidate{Remote: candidate.remote, Subdir: candidate.subdir})
	if err != nil {
		return
	}
	path := rememberedCandidatePath(cacheRoot, source)
	err = os.MkdirAll(filepath.Dir(path), 0o700)
	if err != nil {
		return
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".candidate-*")
	if err != nil {
		return
	}
	tempPath := temp.Name()
	defer func() { _ = os.Remove(tempPath) }()
	_, writeErr := temp.Write(encoded)
	closeErr := temp.Close()
	if writeErr != nil || closeErr != nil {
		return
	}
	_ = os.Rename(tempPath, path)
}
